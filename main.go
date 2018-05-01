// lrt is a live reloading tool for go.
//
// It works by using go list -f '{{join .Deps "\n"}}' to get a list of service
// dependencies, and watching them all using fsnotify.
//
// Care is taken to pause requests while rebuilding is in progress using a
// RWMutex to allow multiple parellel requests or one rebuild. This has the
// nice side-effect that an inflight request will be completed successfully
// before rebuilding starts.
//
// When we run go build we pass -v to get a new list of service dependencies to
// keep the watch graph complete.
//
// To avoid lost requests while the app is booting, we make use of a healthcheck
// and we try and provide useful error messages (with hints!) for common errors.
package main

import (
	"flag"
	"fmt"
	"go/build"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	shellwords "github.com/mattn/go-shellwords"
	"github.com/rjeczalik/notify"
)

// raw arguments
var (
	listenFlag      = flag.String("listen", "localhost:3000", "where lrt should listen")
	serviceFlag     = flag.String("service", "", "where your service listens (if it does not listen on $PORT)")
	buildArgsFlag   = flag.String("build-args", "", "extra flags to pass to go build")
	cmdArgsFlag     = flag.String("cmd-args", "", "extra flags to pass to the service executable")
	healthCheckFlag = flag.String("health-check", "/", "the path lrt pings to check your service has started")
	timeoutFlag     = flag.Duration("health-check-timeout", 10*time.Second, "how long to wait for the service to boot before assuming it has errored")
)

// parsed arguments, see mustParseArgs
var (
	packageName    string
	listenURL      *url.URL
	serviceURL     *url.URL
	healthCheckURL *url.URL

	buildArgs []string
	cmdArgs   []string
)

// internal state
var (
	proxyLock     sync.RWMutex
	errorResponse []byte
	builtOnce     bool

	service *exec.Cmd
	waiter  sync.WaitGroup
	tmpFile *os.File

	eventChan  chan notify.EventInfo
	watchedDir = map[string]bool{}
)

// main
func main() {

	mustParseArgs()
	defer os.Remove(tmpFile.Name())

	fmt.Printf("lrt: listening on %s (forwarding to %s)\n", listenURL, serviceURL)

	go rebuildOnChange()

	proxy := &blockingProxy{httputil.NewSingleHostReverseProxy(serviceURL)}

	err := http.ListenAndServe(listenURL.Host, proxy)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lrt: "+err.Error())
		if strings.Contains(err.Error(), "address already in use") {
			fmt.Fprintf(os.Stderr, "     hint: Are you already running a development server somewhere else?\n")
			fmt.Fprintf(os.Stderr, "           if so try `lsof -i:%v` to find the process id\n", listenURL.Port())
		}
		os.Exit(1)
	}
}

type blockingProxy struct {
	proxy http.Handler
}

func (b *blockingProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	proxyLock.RLock()
	defer proxyLock.RUnlock()

	// on first boot we want to ensure we don't pass any
	// requests through until we've built the service.
	for !builtOnce {
		proxyLock.RUnlock()
		time.Sleep(100 * time.Millisecond)
		proxyLock.RLock()
	}

	if errorResponse != nil {
		w.WriteHeader(http.StatusBadGateway)
		w.Write(errorResponse)
		return
	}

	b.proxy.ServeHTTP(w, r)
}

// rebuildOnChange sets up all the watches and the rebuilder
func rebuildOnChange() {
	eventChan = make(chan notify.EventInfo)
	defer notify.Stop(eventChan)
	defer close(eventChan)

	rebuilder := debounceCallable(100*time.Millisecond, rebuild)
	go rebuilder()

	go func() {

		shutdownCh := make(chan os.Signal, 1)
		signal.Notify(shutdownCh, syscall.SIGTERM)
		signal.Notify(shutdownCh, syscall.SIGINT)
		<-shutdownCh

		proxyLock.Lock()
		defer proxyLock.Unlock()

		stopRunningService()
		waiter.Wait()
		os.Exit(0)
	}()

	for {
		select {
		// watch for events
		case ev := <-eventChan:
			if strings.HasSuffix(ev.Path(), ".go") && !strings.HasSuffix(ev.Path(), "_test.go") {
				go rebuilder()
			}
		}
	}
}

// rebuild rebuilds the package, and restarts it.
// if there are compilation errors it sets errorResponse.
// if new packages have been added, it watches them
func rebuild() {
	proxyLock.Lock()
	defer proxyLock.Unlock()

	if builtOnce {
		fmt.Printf("lrt: rebuilding...\n")
	}

	// Usually we can rely on `go build -v` to give us a list of package names,
	// but it will only list packages that need recompiling.
	// On first run, or if the last build failed, we get all the dependencies and
	// watch them explicitly.
	if !builtOnce || errorResponse != nil {
		output, err := exec.Command("go", "list", "-f", `{{ join .Deps  "\n"}}`, packageName).CombinedOutput()
		if err != nil {
			if _, ok := err.(*exec.ExitError); ok {
				fmt.Fprint(os.Stderr, "lrt: "+string(output))
			} else {
				fmt.Fprint(os.Stderr, "lrt: "+err.Error())
			}
			os.Exit(1)
		}

		watchListedPackages([]byte(packageName))
		watchListedPackages(output)
	}

	builtOnce = true
	errorResponse = nil

	stopRunningService()

	args := append(buildArgs, "-o", tmpFile.Name(), "-v", packageName)
	output, err := exec.Command("go", append([]string{"build"}, args...)...).CombinedOutput()

	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			errorResponse = output
			fmt.Print(string(output))
		} else {
			fmt.Fprint(os.Stderr, "lrt: "+err.Error())
			os.Exit(1)
		}
		return
	}

	watchListedPackages(output)

	// wait for previous service to finish
	waiter.Wait()

	service = exec.Command(tmpFile.Name(), cmdArgs...)
	service.Env = append(os.Environ(), "PORT="+serviceURL.Port())
	service.Stdout = os.Stdout
	service.Stderr = os.Stderr
	err = service.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	exitCh := make(chan bool, 1)
	listeningCh := make(chan bool, 1)

	waiter.Add(1)
	go func() {
		defer waiter.Done()
		service.Wait()
		exitCh <- true
	}()

	go func() {
		for {
			resp, err := http.Get(healthCheckURL.String())
			if err != nil {
				continue
			}
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
				break
			}
		}

		listeningCh <- true
	}()

	select {
	case <-exitCh:
		errorResponse = []byte("lrt: error: service unexpectedly exited before responding to " + healthCheckURL.String() + "\n" +
			"     hint: check the terminal output to see if any errors were logged.\n")
		fmt.Fprintf(os.Stderr, string(errorResponse))

	case <-time.After(*timeoutFlag):
		errorResponse = []byte("lrt: error: service is still not responding on " + healthCheckURL.String() + " after " + (*timeoutFlag).String() + "\n" +
			"     hint: ensure your service listens on $PORT. For example: http.ListenAndServe(\"localhost:\" + os.Getenv(\"PORT\"), nil)\n" +
			"           also, check the terminal output to see if any errors were logged.\n")
		fmt.Fprintf(os.Stderr, string(errorResponse))

	case <-listeningCh:

	}

}

// stopRunningService implements graceful shutdown by sending SIGTERM, waiting up to 10 seconds, and then SIGKILL
func stopRunningService() {
	if service != nil {
		service.Process.Signal(syscall.SIGTERM)
		go func() {
			deadChan := make(chan bool, 1)
			go func() {
				service.Process.Wait()
				deadChan <- true
			}()
			select {
			case <-time.After(10 * time.Second):
				service.Process.Kill()
				service.Process.Wait()
			case <-deadChan:
			}
		}()
	}
}

// watchListedPackages takes a list of newline separated package names,
// such as generated by:
//   go build -v
//   go list -f '{{ join .Deps "\n" }}'
// and adds them to the watch list
func watchListedPackages(output []byte) {

	packages := strings.Split(strings.TrimSpace(string(output)), "\n")

	for _, p := range packages {
		if p == "" {
			continue
		}
		pkg, err := build.Default.Import(p, ".", build.FindOnly)
		if err != nil {
			fmt.Fprintf(os.Stderr, "lrt: "+err.Error(), "\n")
			os.Exit(1)
		}

		if !watchedDir[pkg.Dir] {
			err = notify.Watch(pkg.Dir, eventChan, notify.Write|notify.Remove|notify.Create)
			if err != nil {
				fmt.Fprintf(os.Stderr, "lrt: "+err.Error()+"\n")
				if strings.Contains(err.Error(), "too many open files") {
					fmt.Fprintf(os.Stderr, "     hint: you may need to increase the number of open files you are allowed, try:\n")
					fmt.Fprintf(os.Stderr, "           sudo launchctl limit maxfiles 1000000 1000000\n")
				}
				os.Exit(1)
			}
			watchedDir[pkg.Dir] = true
		}

	}
}

// generateServiceURL asks the kernel for a free open port that is ready to use,
// falling back to 1xxxx where xxxx is the listen port.
// https://github.com/phayes/freeport/blob/master/freeport.go
func generateServiceURL(listenURL *url.URL) *url.URL {
	addr, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(listenURL.Hostname(), "0"))
	if err != nil {
		return &url.URL{Scheme: listenURL.Scheme, Host: net.JoinHostPort(listenURL.Hostname(), "1"+listenURL.Port())}
	}
	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return &url.URL{Scheme: listenURL.Scheme, Host: net.JoinHostPort(listenURL.Hostname(), "1"+listenURL.Port())}
	}
	defer l.Close()
	return &url.URL{Scheme: listenURL.Scheme, Host: net.JoinHostPort(listenURL.Hostname(), strconv.Itoa(l.Addr().(*net.TCPAddr).Port))}
}

// debounceCallable slows down rebuilds in case of a large number of simultaneously file changes
// https://gist.github.com/leolara/d62b87797b0ef5e418cd#gistcomment-2243168
func debounceCallable(interval time.Duration, f func()) func() {
	var timer *time.Timer

	return func() {
		if timer == nil {
			timer = time.NewTimer(interval)

			go func() {
				<-timer.C
				timer.Stop()
				timer = nil
				f()
			}()
		} else {
			timer.Reset(interval)
		}
	}
}

func mustParseArgs() {

	flag.Usage = func() {
		fmt.Print(`Usage: lrt [options] <package>

lrt wraps a go http service and reloads it whenever the source code changes.
lrt acts as a "Live Reload Tool" by proxying requests to the service, queueing
requests where necessary so that your service always appears to be live, and
ensuring that requests never hit an old version of the code.

parameters:
  package
	the go package to build (default ".")

options:
`)
		flag.PrintDefaults()

		fmt.Print(`
lrt listens on localhost:3000 and boots your service with a PORT environment variable set.
Your service should start an HTTP server on the provided port. For more details see:
https://github.com/superhuman/lrt
`)
		os.Exit(2)
	}

	flag.Parse()

	listenURL = argToURL("-listen", listenFlag)

	if *serviceFlag == "" {
		serviceURL = generateServiceURL(listenURL)
	} else {
		serviceURL = argToURL("-service", serviceFlag)
	}

	var err error
	healthCheckURL, err = url.Parse(*healthCheckFlag)
	if err != nil {
		fmt.Printf("lrt: -started-probe %#v is not a valid url. See lrt --help for details\n", *healthCheckFlag)
		os.Exit(1)
	}

	if serviceURL.ResolveReference(healthCheckURL).Host != serviceURL.Host {
		fmt.Printf("lrt: -started-probe %#v is not relative to -service %#v. See lrt --help for details\n", *healthCheckFlag, *serviceFlag)
		os.Exit(1)
	}
	healthCheckURL = serviceURL.ResolveReference(healthCheckURL)

	if len(flag.Args()) == 1 {
		packageName = flag.Args()[0]
	} else {
		packageName = "."
	}

	pkg, err := build.Default.Import(packageName, ".", 0)
	if err != nil {
		if strings.HasPrefix(err.Error(), "cannot find package") {
			fmt.Fprintf(os.Stderr, "lrt: cannot find package %#v\n", packageName)
			_, err = os.Stat(packageName)
			if err == nil {
				fmt.Fprintf(os.Stderr, "     hint: go packages are specified by package name, e.g. \"github.com/superhuman/lrt\"\n")
				fmt.Fprintf(os.Stderr, "           to use a relative directory start with ./, e.g. \"./lrt\"\n")
				if strings.HasSuffix(packageName, ".go") {
					fmt.Fprintf(os.Stderr, "           running individual go files is not yet supported.\n")
				}
			}
			os.Exit(1)

		} else {
			fmt.Fprintln(os.Stderr, "lrt: "+err.Error())
			os.Exit(1)
		}
	}
	if pkg.Name != "main" {
		fmt.Printf("lrt: %#v does not contain package \"main\".\n", packageName)
		os.Exit(1)
	}

	buildArgs, err = shellwords.Parse(*buildArgsFlag)
	if err != nil {
		panic(err) // can only happen if shellwords.ParseBacktick is true, and it isn't
	}

	cmdArgs, err = shellwords.Parse(*cmdArgsFlag)
	if err != nil {
		panic(err) // can only happen if shellwords.ParseBacktick is true, and it isn't
	}

	tmpFile, err = ioutil.TempFile("", "lrt-service")
	if err != nil {
		fmt.Fprintf(os.Stderr, "lrt: "+err.Error())
		os.Exit(1)
	}

}

// argToURL converts a go-style host:port pair into a URL, exiting early if the arg is invalid.
func argToURL(name string, str *string) *url.URL {
	host, port, err := net.SplitHostPort(*str)
	if err != nil {
		fmt.Printf("lrt: %s is invalid. Expected something like \"localhost:3000\" or \":3000\". See lrt --help for details\n", name)
		os.Exit(2)
	}

	return &url.URL{
		Scheme: "http",
		Host:   host + ":" + port,
	}
}
