package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/build"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	dbg "github.com/traefik-contrib/yaegi-debug-adapter"
	"github.com/traefik-contrib/yaegi-debug-adapter/internal/dap"
	"github.com/traefik-contrib/yaegi-debug-adapter/internal/iox"
	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
	"github.com/traefik/yaegi/stdlib/syscall"
	"github.com/traefik/yaegi/stdlib/unrestricted"
	"github.com/traefik/yaegi/stdlib/unsafe"
)

//nolint:gocyclo // TODO must be fixed
func main() {
	var (
		mode          string
		addr          string
		logFile       string
		stopAtEntry   bool
		singleSession bool
		asString      bool
		tags          string
		noAutoImport  bool
	)

	// The following flags are initialized from environment.
	useSyscall, _ := strconv.ParseBool(os.Getenv("YAEGI_SYSCALL"))
	useUnrestricted, _ := strconv.ParseBool(os.Getenv("YAEGI_UNRESTRICTED"))
	useUnsafe, _ := strconv.ParseBool(os.Getenv("YAEGI_UNSAFE"))

	flag.StringVar(&mode, "mode", "stdio", "Listening mode, stdio|net")
	flag.StringVar(&addr, "addr", "tcp://localhost:16348", "Net address to listen on, must be a TCP or Unix socket URL")
	flag.StringVar(&logFile, "log", "", "Log protocol messages to a file")
	flag.BoolVar(&stopAtEntry, "stop-at-entry", false, "Stop at program entry")
	flag.BoolVar(&singleSession, "single-session", true, "Run a single debug session and exit once it terminates")
	flag.BoolVar(&asString, "as-string", false, "Use Eval instead of EvalPath")
	flag.StringVar(&tags, "tags", "", "set a list of build tags")
	flag.BoolVar(&useSyscall, "syscall", useSyscall, "include syscall symbols")
	flag.BoolVar(&useUnrestricted, "unrestricted", useUnrestricted, "include unrestricted symbols")
	flag.BoolVar(&useUnsafe, "unsafe", useUnsafe, "include unsafe symbols")
	flag.BoolVar(&noAutoImport, "noautoimport", false, "do not auto import pre-compiled packages. Import names that would result in collisions (e.g. rand from crypto/rand and rand from math/rand) are automatically renamed (crypto_rand and math_rand)")
	flag.Usage = func() {
		fmt.Println("Usage: yaegi debug [options] <path> [args]")
		fmt.Println("Options:")
		flag.PrintDefaults()
	}

	flag.Parse()
	args := flag.Args()

	if len(args) == 0 {
		log.Fatal("missing script path")
	}

	shouldAutoImport := !noAutoImport
	newInterp := func(opts interp.Options) (*interp.Interpreter, error) {
		opts.GoPath = build.Default.GOPATH
		opts.BuildTags = strings.Split(tags, ",")
		i := interp.New(opts)
		if err := i.Use(stdlib.Symbols); err != nil {
			return nil, err
		}
		if err := i.Use(interp.Symbols); err != nil {
			return nil, err
		}
		if useSyscall {
			if err := i.Use(syscall.Symbols); err != nil {
				return nil, err
			}
			// Using a environment var allows a nested interpreter to import the syscall package.
			if err := os.Setenv("YAEGI_SYSCALL", "1"); err != nil {
				return nil, err
			}
		}
		if useUnsafe {
			if err := i.Use(unsafe.Symbols); err != nil {
				return nil, err
			}
			if err := os.Setenv("YAEGI_UNSAFE", "1"); err != nil {
				return nil, err
			}
		}
		if useUnrestricted {
			// Use of unrestricted symbols should always follow stdlib and syscall symbols, to update them.
			if err := i.Use(unrestricted.Symbols); err != nil {
				return nil, err
			}
			if err := os.Setenv("YAEGI_UNRESTRICTED", "1"); err != nil {
				return nil, err
			}
		}
		if shouldAutoImport {
			i.ImportUsed()
		}

		return i, nil
	}

	errch := make(chan error)
	go func() {
		for err := range errch {
			fmt.Printf("ERR %v\n", err)
		}
	}()
	defer close(errch)

	opts := &dbg.Options{
		StopAtEntry:    stopAtEntry,
		NewInterpreter: newInterp,
		Errors:         errch,
		SrcPath:        args[0],
	}

	var adp *dbg.Adapter
	if asString {
		b, err := ioutil.ReadFile(args[0])
		if err != nil {
			//nolint:gocritic // TODO must be fixed
			log.Fatal(err)
		}

		adp = dbg.NewEvalAdapter(string(b), opts)
	} else if src, ok := isScript(args[0]); ok {
		adp = dbg.NewEvalAdapter(src, opts)
	} else {
		shouldAutoImport = false
		adp = dbg.NewEvalPathAdapter(args[0], opts)
	}

	var l net.Listener
	switch mode {
	case "stdio":
		l = iox.NewStdio()

	case "net":
		u, err := url.Parse(addr)
		if err != nil {
			log.Fatal(err)
		}

		var addr string
		if u.Scheme == "unix" {
			addr = u.Path
			if _, err = os.Stat(addr); err == nil {
				// Remove any pre-existing connection
				_ = os.Remove(addr)
			}

			// Remove when done
			defer func() { _ = os.Remove(addr) }()
		} else {
			addr = u.Host
		}
		l, err = net.Listen(u.Scheme, addr)
		if err != nil {
			log.Fatal(err)
		}

	default:
		log.Fatalf("Invalid mode %q", mode)
	}

	srv := dap.NewServer(l, adp)

	var lf io.Writer
	if logFile == "-" {
		lf = os.Stderr
	} else if logFile != "" {
		f, err := os.Create(logFile)
		if err != nil {
			log.Fatalf("log: %v", err)
		}
		defer func() { _ = f.Close() }()
		lf = f
	}

	if singleSession {
		s, c, err := srv.Accept()
		if err != nil {
			log.Fatal(err)
		}
		defer func() { _ = c.Close() }()

		s.Debug(lf)
		err = s.Run()
		if err != nil {
			log.Fatal(err)
		}
		return
	}

	for {
		s, c, err := srv.Accept()
		if err != nil {
			log.Fatal(err)
		}

		//nolint:errcheck,staticcheck // TODO must be fixed
		defer c.Close()

		lf, addr := lf, c.RemoteAddr()
		if lf != nil {
			prefix := []byte(fmt.Sprintf("{%v}", addr))
			ogLF := lf
			lf = iox.WriterFunc(func(b []byte) (int, error) {
				n, err := ogLF.Write(append(prefix, b...))
				if n < len(prefix) {
					n = 0
				} else {
					n -= len(prefix)
				}
				return n, err
			})
		}

		s.Debug(lf)
		err = s.Run()
		if err != nil {
			fmt.Printf("{%v} ERR %v\n", addr, err)
		}
	}
}

func isScript(path string) (src string, ok bool) {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return "", false
	}

	if !bytes.HasPrefix(b, []byte("#!")) {
		return "", false
	}

	b[0], b[1] = '/', '/'
	return string(b), true
}
