lrt is a "live reloading tool" for go http services. It provides reliable hot
code reloading so that after you change the source code future requests will
hit the new version of the code.

## Installation & Usage

1. Install the tool
  ```
  go install github.com/superhuman/lrt
  ```

2. Ensure your service respects the PORT environment variable. lrt will set PORT to
a temporary port when running your service, and forward all requests to it.
  ```
  ...
  http.ListenAndServe("localhost:" + os.Getenv("PORT"), nil)
  ...
  ```

3. Run your service with lrt

  ```
  cd $GOPATH/src/github.com/example/service
  lrt
  ```

Make development requests to http://localhost:3000/. 

### Configuration

```
Usage: lrt [options] <package>

parameters:
  package
	the go package to build (default ".")

options:
  -build-args string
    	extra flags to pass to go build
  -cmd-args string
    	extra flags to pass to the service executable
  -health-check string
    	the path lrt pings to check your service has started (default "/")
  -health-check-timeout duration
    	how long to wait for the service to boot before assuming it has errored (default 10s)
  -listen string
    	where lrt should listen (default "localhost:3000")
  -service string
    	where your service listens (if it does not listen on $PORT)

lrt listens on localhost:3000 and boots your service with a PORT environment variable set.
Your service should start an HTTP server on the provided port. For more details see:
https://github.com/superhuman/lrt
```

## How it works

lrt uses fsnotify to monitor the filesytem for changes and rebuilds and
restarts your service when any files change. lrt is designed to proxy all
requests to your service so that these restarts are transparent to calling
code.

### Building

When started, and when a change is detected, lrt builds your service using `go
build -o lrt-service-XXX -i -v package`. The `-i` accelerates rebuilds, and
`-v` is used to track dependencies.  `-o` is always set to a temporary file
that is deleted when lrt exits. To customize other arguments to go build, you can
pass them as `--build-args`.

For example to set ld flags on the go executable, you could do something like:

```
lrt --build-args="-ldflags=\"-X github.com/superhuman/example.Revision=3\""
```

If the executable fails to build, then lrt will output the build error to
stdout, and will also respond to any http requests with a 502 error containing
the build error for easy debugging.

lrt tracks all dependencies of the code, including those in `vendor/` and in
other parts of your $GOPATH.

### Running

After the executable has built successfully, it will be run with the PORT
environment variables set to a temporary value. This value is picked freshly on
every boot of lrt to ensure that you don't have any conflicts. If you need to
pass command line arguments to your service you can do so:

```
lrt -cmd-args="--debug --database-url=\"psql://localhost/test\""
# lrt will run your service as though you'd typed:
PORT=XXX service --debug --database-url="psql://localhost/test"
```

If your service ignores the PORT environment variable, andalways listens on a
particular port you can tell lrt where to find it by passing the `-service`
parameter.

```
lrt -service localhost:8080
# lrt will listen on port 3000 and forward requests to 8080
```

To access the service reliably you should make requests to the port that lrt is
listening on. This defaults to port 3000, but you can change this if you are using
that port for something else.

```
lrt -listen localhost:80000 -service localhost:8080
# lrt will listen on port 8000 and forward requests to 8080
```

### Health checks

In order to avoid dropping requests while your service boots, lrt will ping a
healthcheck url until it gets a 200 response. By default the healthcheck is "/", but you
can override this with:

```
lrt --health-check "/ping"
```

If your app exits before the health check returns 200, or if more than 10
seconds have passed, then lrt will output an error and start responding to all
requests with an error for easy debugging. The terminal output should contain any
errors that your service has logged.

```
lrt: error: service unexpectedly exited before responding to http://localhost:56216/
     hint: check the terminal output to see if any errors were logged.
```

If your app takes longer than 10 seconds to load then you can extend the timeout with:

```
lrt --health-check "/ping" --health-check-timeout 30s
```

If the timeout is exceeded, you will see:

```
lrt: error: service is still not responding on http://localhost:56492/ after 500ms
     hint: ensure your service listens on $PORT. For example: http.ListenAndServe("localhost:" + os.Getenv("PORT"), nil)
           also, check the terminal output to see if any errors were logged.
```

The most common reason for this is that your service is booting, but it is not
listening on the correct port. Ensure that your service listens on
`"localhost:" + os.Getenv("PORT")`.

### Termination

lrt will try to shut down your service cleanly by first sending it a SIGTERM,
and then waiting 10 seconds before sending a SIGKILL. To avoid doubly handled
requests lrt will wait for the old service to exit before starting the new one,
so if things are slower than they should be, check how long your service takes
to shut down.

## Limitations

lrt currently assumes that the build environment does not change between when you
compiled lrt and when you are compiling other programs using lrt.

If you are seeing problems with build failing because of missing packages or
mis-matched paths reinstall lrt with `go install github.com/superhuman/lrt`.

The only exception to the above limitation is that if the go version has
changed `lrt` will recompile itself and then run the new version of `lrt`
automatically.

## Credits etc.

lrt is inspired by [gin](https://github.com/codegangsta/gin), which was an
earlier, buggier, version of the idea.

Bug reports and pull requests are welcome! The general philosophy of lrt is
that it should firstly be as correct as possible, and secondly as easy to use
as possible. Thirdly, and least importantly, it should support as many
use-cases as possible. In other words, lrt should do the right thing
automatically. In the case we can't keep everyone happy, more options can be
added.

lrt is licensed under the MIT license, see LICENSE.MIT for details.
