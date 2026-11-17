// The forge binary: bind an ephemeral port, print it for the launcher,
// and serve until the trial ends. The launcher (main.ts) owns the env
// lifecycle; this process only serves HTTP and exits on SIGTERM/SIGINT.

package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	token := trimJSSpace(os.Getenv("MAKEFUN_SHOP_TOKEN"))
	store := NewStore()
	d := &dispatcher{store: store, token: token}
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "[forge] listen:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("forge listening on port %d\n", port)
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		ln.Close()
		os.Exit(0)
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go serveConn(d, conn)
	}
}
