// Command hub serves the btop hub page and receives agent samples.
//
//	hub                 run the server (configured by HUB_* environment variables)
//	hub health          exit 0 if the local server answers (container health check)
//	hub token           print the shared agent token
//	hub forget ID       remove a machine and its history
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"btophub/internal/hub"
	"btophub/internal/proto"
	"btophub/scripts"
	"btophub/web"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	listen := env("HUB_LISTEN", ":28677")
	dataDir := env("HUB_DATA", "/data")
	port := listen[strings.LastIndexByte(listen, ':')+1:]
	local := "http://127.0.0.1:" + port

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "health":
			c := &http.Client{Timeout: 3 * time.Second}
			resp, err := c.Get(local + "/healthz")
			if err != nil || resp.StatusCode != http.StatusOK {
				os.Exit(1)
			}
			return
		case "token":
			t, err := hub.ReadToken(dataDir)
			if err != nil {
				log.Fatal(err)
			}
			fmt.Println(t)
			return
		case "forget":
			if len(os.Args) < 3 {
				log.Fatal("usage: hub forget MACHINE_ID")
			}
			t, err := hub.ReadToken(dataDir)
			if err != nil {
				log.Fatal(err)
			}
			req, _ := http.NewRequest(http.MethodDelete, local+"/api/machines/"+os.Args[2], nil)
			req.Header.Set("Authorization", "Bearer "+t)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				log.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			fmt.Print(string(b))
			if resp.StatusCode != http.StatusOK {
				os.Exit(1)
			}
			return
		case "version":
			fmt.Println("btop hub", proto.Version)
			return
		default:
			log.Fatalf("unknown command %q (health, token, forget ID, version)", os.Args[1])
		}
	}

	runtime.GOMAXPROCS(2)
	debug.SetGCPercent(50)
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(24 << 20)
	}

	allow, err := hub.ParseAllow(env("HUB_ALLOW", ""))
	if err != nil {
		log.Fatal(err)
	}
	if len(allow) == 0 {
		log.Print("warning: HUB_ALLOW is empty, so only loopback can connect")
	}
	var order []string
	for _, id := range strings.Split(env("HUB_ORDER", ""), ",") {
		if id = strings.TrimSpace(id); id != "" {
			order = append(order, id)
		}
	}
	maxM, _ := strconv.Atoi(env("HUB_MAX_MACHINES", "8"))
	h, err := hub.New(hub.Config{
		DataDir: dataDir, Order: order, MaxMachines: maxM, MaxViewers: 32,
		PublicURL: env("HUB_URL", ""), DistDir: env("HUB_DIST", "/dist"),
	})
	if err != nil {
		log.Fatal(err)
	}
	srv, err := hub.NewServer(h, allow, hub.Assets{Web: web.FS, Scripts: scripts.FS})
	if err != nil {
		log.Fatal(err)
	}
	hs := &http.Server{
		Addr:              listen,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	log.Printf("btop hub %s listening on %s; allowed networks %v", proto.Version, listen, allow)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
