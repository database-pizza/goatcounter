package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	_ "time/tzdata"

	"zgo.at/errors"
	"zgo.at/goatcounter/v2"
	"zgo.at/goatcounter/v2/db/migrate/gomig"
	"zgo.at/goatcounter/v2/pkg/log"
	"zgo.at/goatcounter/v2/pkg/pizzasql"
	"zgo.at/jfmt"
	"zgo.at/json"
	"zgo.at/slog_align"
	"zgo.at/zdb"
	"zgo.at/zdb-drivers/go-sqlite3"
	_ "zgo.at/zdb-drivers/pq"
	"zgo.at/zdb/drivers"
	"zgo.at/zli"
	"zgo.at/zstd/zfs"
	"zgo.at/zstd/zruntime"
	"zgo.at/zstd/zslice"
)

func init() {
	errors.Package = "zgo.at/goatcounter/v2"
}

type command func(f zli.Flags, ready chan<- struct{}, stop chan struct{}) error

func main() {
	// Linux doesn't allow some environment variables to be set if any
	// capability bits (such as cap_net_bind_service) are set, so also read from
	// GOATCOUNTER_TMPDIR
	if v, ok := os.LookupEnv("GOATCOUNTER_TMPDIR"); ok {
		os.Setenv("TMPDIR", v)
		os.Unsetenv("GOATCOUNTER_TMPDIR")
	}

	var (
		f     = zli.NewFlags(os.Args)
		ready = make(chan struct{}, 1)
		stop  = make(chan struct{}, 1)
	)
	slog.SetDefault(slog.New(slog_align.NewAlignedHandler(os.Stdout, nil)))
	cmdMain(f, ready, stop)
}

var mainDone sync.WaitGroup

func cmdMain(f zli.Flags, ready chan<- struct{}, stop chan struct{}) {
	mainDone.Add(1)
	defer mainDone.Done()

	cmd, err := f.ShiftCommand("help", "version", "serve", "import",
		"dashboard", "db", "monitor",
		"saas", "goat")
	if zslice.ContainsAny(f.Args, "-h", "-help", "--help") {
		f.Args = append([]string{cmd}, f.Args...)
		cmd = "help"
	}
	if err != nil && !errors.Is(err, zli.ErrCommandNoneGiven{}) {
		zli.Errorf(usage[""])
		zli.Errorf("%s", err)
		zli.Exit(1)
		return
	}

	var run command
	switch cmd {
	default:
		zli.Errorf(usage[""])
		zli.Errorf("unknown command: %q", cmd)
		zli.Exit(1)
	case "", "help":
		run = cmdHelp
	case "version":
		var (
			jsonFlag = f.Bool(false, "json")
		)
		if err := f.Parse(); err != nil {
			zli.F(err)
		}
		if jsonFlag.Bool() {
			j, err := json.Marshal(map[string]any{
				"version": goatcounter.Version,
				"go":      runtime.Version(),
				"GOOS":    runtime.GOOS,
				"GOARCH":  runtime.GOARCH,
				"race":    zruntime.Race,
				"cgo":     zruntime.CGO,
			})
			if err != nil {
				panic(err)
			}
			jj, err := jfmt.NewFormatter(80, "", "  ").FormatString(string(j))
			if err != nil {
				panic(err)
			}
			fmt.Print(jj)
		} else {
			fmt.Printf("version=%s; go=%s; GOOS=%s; GOARCH=%s; race=%t; cgo=%t\n",
				goatcounter.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH,
				zruntime.Race, zruntime.CGO)
		}
		zli.Exit(0)
		return

	case "db", "database":
		run = cmdDB
	case "serve":
		run = func(f zli.Flags, ready chan<- struct{}, stop chan struct{}) error {
			return cmdServe(f, ready, stop, false)
		}
	case "saas":
		run = func(f zli.Flags, ready chan<- struct{}, stop chan struct{}) error {
			return cmdServe(f, ready, stop, true)
		}
	case "monitor":
		run = cmdMonitor
	case "import":
		run = cmdImport
	case "dashboard":
		// Wrap as this also doubles as an example, and these flags just obscure
		// things.
		run = func(f zli.Flags, ready chan<- struct{}, stop chan struct{}) error {
			defer func() { ready <- struct{}{} }()
			return cmdDashboard(f)
		}
	case "goat":
		run = func(f zli.Flags, ready chan<- struct{}, stop chan struct{}) error {
			defer func() { ready <- struct{}{} }()
			fmt.Print(goat[1:])
			return nil
		}

	case "create":
		flags := os.Args[2:]
		for i, ff := range flags {
			if ff == "-domain" {
				flags[i] = "-vhost"
			}
			if strings.HasPrefix(ff, "-domain=") {
				flags[i] = "-vhost=" + ff[8:]
			}
		}
		fmt.Fprintf(zli.Stderr,
			"The create command is moved to \"goatcounter db create site\"\n\n\t$ goatcounter db create site %s\n",
			strings.Join(flags, " "))
		zli.Exit(5)
		return
	}

	err = run(f, ready, stop)
	if err != nil {
		if !log.HasDebug("cli-trace") {
			for {
				var s *errors.StackErr
				if !errors.As(err, &s) {
					break
				}
				err = s.Unwrap()
			}
		}

		c := 1
		var stErr interface {
			Code() int
			Error() string
		}
		if errors.As(err, &stErr) {
			c = stErr.Code()
			if c > 255 { // HTTP error.
				c = 1
			}
		}

		if c == 0 {
			if err.Error() != "" {
				fmt.Fprintln(zli.Stdout, err.Error())
			}
			zli.Exit(0)
		}
		zli.Errorf(err)
		zli.Exit(c)
		return
	}
	zli.Exit(0)
}

// registerPizzaSQL installs the PizzaSQL zdb driver when connect targets the
// managed service. Registering on demand keeps ordinary local SQLite
// connections on go-sqlite3.
func registerPizzaSQL(connect string) {
	if pizzasql.IsConnectString(connect) {
		pizzasql.Register()
	}
}

// effectivePool resolves the max-open and max-idle pool sizes for a
// connection. An explicitly supplied -dbconn always wins. Otherwise PizzaSQL
// uses its bounded 2,2 default and every other engine falls back to zdb's own
// 16,4 default (returned as 0,0).
func effectivePool(connect, dbConn string, dbConnSet bool) (open, idle int, err error) {
	if dbConnSet {
		openS, idleS, ok := strings.Cut(dbConn, ",")
		if !ok {
			return 0, 0, errors.New("-dbconn flag: must be as max_open,max_idle")
		}
		open, err = strconv.Atoi(openS)
		if err != nil {
			return 0, 0, fmt.Errorf("-dbconn flag: %w", err)
		}
		idle, err = strconv.Atoi(idleS)
		if err != nil {
			return 0, 0, fmt.Errorf("-dbconn flag: %w", err)
		}
		return open, idle, nil
	}

	// The pgproxy enforces a per-organization connection quota, so PizzaSQL
	// keeps a small pool even though serve and monitor default -dbconn to the
	// generic 16,4.
	if pizzasql.IsConnectString(connect) {
		return pizzasql.MaxOpenConns, pizzasql.MaxIdleConns, nil
	}
	return 0, 0, nil
}

// redactConnect returns a connect string that is safe to put in a user-facing
// message. PizzaSQL connect strings carry the managed API key as their
// password, so they are never echoed in full.
func redactConnect(connect string) string {
	return pizzasql.RedactConnect(connect)
}

// redactDBError removes a PizzaSQL connect string from an error before it can
// reach a user. zdb's NotExistError embeds the raw connection string, which for
// PizzaSQL includes the managed API key. The error keeps its type so callers
// that branch on NotExistError still work.
func redactDBError(connect string, err error) error {
	return pizzasql.RedactError(connect, err)
}

func connectDB(connect, dbConn string, dbConnSet bool, migrate []string, create, dev bool) (zdb.DB, context.Context, error) {
	if strings.Contains(connect, "://") && !strings.Contains(connect, "+") {
		connect = strings.Replace(connect, "://", "+", 1)
		log.Warnf(context.Background(), `the connection string for -db changed from "engine://connectString"`+
			` to "engine+connectString"; the ://-variant will work for now, but will be removed in a future release`)
	}
	registerPizzaSQL(connect)

	open, idle, err := effectivePool(connect, dbConn, dbConnSet)
	if err != nil {
		return nil, nil, err
	}

	fsys, err := zfs.EmbedOrDir(goatcounter.DB, "db", dev)
	if err != nil {
		return nil, nil, err
	}

	sqlite3.DefaultHook(goatcounter.SQLiteHook)

	db, err := zdb.Connect(context.Background(), zdb.ConnectOptions{
		Connect:      connect,
		Files:        fsys,
		Migrate:      migrate,
		GoMigrations: gomig.Migrations,
		Create:       create,
		MaxOpenConns: open,
		MaxIdleConns: idle,
		MigrateLog:   func(name string) { log.Infof(context.Background(), "running migration %q", name) },
	})
	var pErr *zdb.PendingMigrationsError
	if errors.As(err, &pErr) {
		log.Warnf(context.Background(), "%s; continuing but things may be broken", err)
		err = nil
	}

	// TODO: maybe ask for confirmation here?
	var cErr *drivers.NotExistError
	if errors.As(err, &cErr) {
		if pizzasql.IsConnectString(connect) {
			err = errors.New("pizzasql database does not exist or is empty.\n" +
				"Add the -createdb flag to create this database if you're sure this is the right location")
		} else if cErr.DB == "" {
			err = fmt.Errorf("%s database at %q exists but is empty.\n"+
				"Add the -createdb flag to create this database if you're sure this is the right location",
				cErr.Driver, redactConnect(connect))
		} else {
			err = fmt.Errorf("%s database at %q doesn't exist.\n"+
				"Add the -createdb flag to create this database if you're sure this is the right location",
				cErr.Driver, cErr.DB)
		}
	}
	if err != nil {
		return nil, nil, redactDBError(connect, err)
	}

	// Insert/update languages. For PostgreSQL this adds ~120ms startup time,
	// which isn't huge but just large enough to be a tad annoying on dev. So do
	// it in the background as this data isn't critical. For SQLite we don't
	// need to do this as it's just ~7ms there (also harder to do fully correct
	// due to SQLite's concurrency limitations).
	ins := func() {
		langs, err := fs.ReadFile(goatcounter.DB, "db/languages.sql")
		if err != nil {
			log.Errorf(context.Background(), "unable to populate languages: %s", err)
		}
		err = db.Exec(context.Background(), string(langs))
		if err != nil {
			log.Errorf(context.Background(), "unable to populate languages: %s", err)
		}
	}
	if db.SQLDialect() == zdb.DialectPostgreSQL && !log.HasDebug("sql") {
		go ins()
	} else {
		ins()
	}

	if log.HasDebug("sql-query") {
		db = zdb.NewLogDB(db, os.Stderr, zdb.DumpQuery|zdb.DumpLocation, "")
	} else if log.HasDebug("sql-result") {
		db = zdb.NewLogDB(db, os.Stderr, zdb.DumpQuery|zdb.DumpLocation|zdb.DumpResult, "")
	}
	return db, goatcounter.NewContext(context.Background(), db), nil
}

func setupLog(dev, asJSON bool, debug []string) {
	o := &slog.HandlerOptions{
		// Our log package takes care of suppressing debug logs.
		Level:     slog.LevelDebug,
		AddSource: true,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == "module" || a.Key == "_err" {
				return slog.Attr{}
			}
			return a
		},
	}
	var handler slog.Handler
	if asJSON {
		handler = slog.NewJSONHandler(os.Stdout, o)
	} else {
		h := slog_align.NewAlignedHandler(os.Stdout, o)
		if !dev {
			h.SetTimeFormat("Jan _2 15:04:05 ")
		}
		handler = h
	}

	log.SetDebug(debug)
	slog.SetDefault(slog.New(handler))
}
