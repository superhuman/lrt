package main

import (
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

var baseListenURL = &url.URL{Scheme: "http", Host: "localhost:3000"}

type Empty struct{}

var packagePath = reflect.TypeOf(Empty{}).PkgPath()
var testPackagePath = packagePath + "/test"

var executable string

func init() {
	path, err := ioutil.TempFile("", "lrt-test")
	if err != nil {
		panic(err)
	}
	executable = path.Name()
	cmd := exec.Command("go", "build", "-o", executable, packagePath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		panic(err)
	}
}

func startLrtForTests(t *testing.T, args ...string) (*url.URL, func()) {
	listenURL := generateServiceURL(baseListenURL)

	args = append(args, "-listen", listenURL.Host, testPackagePath)

	cmd := exec.Command(executable, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Start()
	if err != nil {
		t.Fatal(err)
	}

	listeningCh := make(chan bool, 1)
	var reterr error

	go func() {
		for {
			resp, err := net.Dial("tcp", listenURL.Host)
			if err != nil {
				if !strings.HasSuffix(err.Error(), "connection refused") {
					reterr = err
					break
				}
				continue
			}

			resp.Close()

			break
		}
		listeningCh <- true
	}()

	select {
	case <-listeningCh:
		if reterr != nil {
			t.Fatal(reterr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal(fmt.Errorf("timeout: lrt did not boot in tests"))
	}

	return listenURL, func() {
		err := cmd.Process.Signal(syscall.SIGTERM)
		if err != nil {
			panic(err)
		}
		cmd.Process.Wait()
	}
}

func getStringResponse(t *testing.T, url *url.URL) string {

	resp, err := http.Get(url.String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return string(body)
}

func waitForFsNotify() {
	time.Sleep(100 * time.Millisecond)
}

func TestLrt(t *testing.T) {
	listenURL, stop := startLrtForTests(t)
	defer stop()

	response := getStringResponse(t, listenURL)

	if response != "lrt/test: OK" {
		t.Errorf("Got unexpected response from lrt: %s", response)
	}
}

func TestLrt_Rebuild(t *testing.T) {
	listenURL, stop := startLrtForTests(t)
	defer stop()

	response := getStringResponse(t, listenURL)

	if response != "lrt/test: OK" {
		t.Errorf("Got unexpected response from lrt: %s", response)
	}

	defer os.Remove("test/override.go")
	ioutil.WriteFile("test/override.go", []byte(
		`package main

		 func init() {
		 	response = "lrt/test: OVERRIDE"
		 }`),
		0644)

	waitForFsNotify()

	response = getStringResponse(t, listenURL)
	if response != "lrt/test: OVERRIDE" {
		t.Errorf("Got unexpected response from lrt: %s", response)
	}
}

func TestLrt_BuildError(t *testing.T) {
	defer os.Remove("test/override.go")
	ioutil.WriteFile("test/override.go", []byte(
		`package main syntax error`),
		0644)

	listenURL, stop := startLrtForTests(t)
	defer stop()

	response := getStringResponse(t, listenURL)

	if !strings.Contains(response, "test/override.go:1:14: syntax error: unexpected syntax") {
		t.Errorf("Got unexpected response from lrt: %s", response)
	}

	ioutil.WriteFile("test/override.go", []byte(
		`package main`),
		0644)

	waitForFsNotify()

	response = getStringResponse(t, listenURL)
	if response != "lrt/test: OK" {
		t.Errorf("Got unexpected response from lrt: %s", response)
	}
}

func TestLrt_BootError(t *testing.T) {
	defer os.Remove("test/override.go")
	ioutil.WriteFile("test/override.go", []byte(
		`package main
		
		func init() {
			panic("oops")
		}
		`),
		0644)

	listenURL, stop := startLrtForTests(t)
	defer stop()

	response := getStringResponse(t, listenURL)

	if !strings.Contains(response, "lrt: error: service unexpectedly exited before responding") {
		t.Errorf("Got unexpected response from lrt: %s", response)
	}

	ioutil.WriteFile("test/override.go", []byte(
		`package main`),
		0644)

	waitForFsNotify()

	response = getStringResponse(t, listenURL)
	if response != "lrt/test: OK" {
		t.Errorf("Got unexpected response from lrt: %s", response)
	}
}

func TestLrt_BootTimeout(t *testing.T) {
	defer os.Remove("test/override.go")
	ioutil.WriteFile("test/override.go", []byte(
		`package main

		import "time"
		
		func init() {
			time.Sleep(1 * time.Second)
		}
		`),
		0644)

	listenURL, stop := startLrtForTests(t, "-health-check-timeout", "500ms")
	defer stop()

	response := getStringResponse(t, listenURL)

	if !strings.Contains(response, "lrt: error: service is still not responding") {
		t.Errorf("Got unexpected response from lrt: %s", response)
	}

	ioutil.WriteFile("test/override.go", []byte(
		`package main`),
		0644)

	waitForFsNotify()

	response = getStringResponse(t, listenURL)
	if response != "lrt/test: OK" {
		t.Errorf("Got unexpected response from lrt: %s", response)
	}
}

func TestLrt_ServiceArg(t *testing.T) {

	anotherURL := generateServiceURL(baseListenURL)

	listenURL, stop := startLrtForTests(t, "-cmd-args", "-override-port "+anotherURL.Port(), "-service", anotherURL.Host)
	defer stop()

	response := getStringResponse(t, listenURL)
	if response != "lrt/test: OK" {
		t.Errorf("Got unexpected response from lrt: %s", response)
	}

	response = getStringResponse(t, anotherURL)
	if response != "lrt/test: OK" {
		t.Errorf("Got unexpected response from lrt/test: %s", response)
	}
}
