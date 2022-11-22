package http_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/packethost/pkg/log"
	. "github.com/tinkerbell/hegel/internal/http"
)

// TestServe validates the Serve function does in-fact serve a functional HTTP server with the
// desired handler.
func TestServe(t *testing.T) {
	logger, err := log.Init(t.Name())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mux http.ServeMux
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "Hello, world!")
	})

	go Serve(ctx, logger, fmt.Sprintf(":%d", 8080), &mux)

	resp, err := http.Get("http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatal("expected status code 200")
	}

	var buf bytes.Buffer
	io.Copy(&buf, resp.Body)

	if buf.String() != "Hello, world!" {
		t.Fatal("expected body to be 'Hello, world!'")
	}
}

func TestServerFailure(t *testing.T) {
	logger, err := log.Init(t.Name())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var mux http.ServeMux
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "Hello, world!")
	})
	n, err := net.Listen("tcp", ":8181")
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if err := Serve(ctx, logger, fmt.Sprintf(":%d", 8181), &mux); err == nil {
		t.Fatal("expected error")
	}
}
