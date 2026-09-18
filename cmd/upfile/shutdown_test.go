package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

type shutdownResponse struct {
	body string
	err  error
}

type shutdownTestServer struct {
	*http.Server
	release         chan struct{}
	shutdownStarted chan struct{}
	handled         chan error
	response        chan shutdownResponse
}

func newShutdownTestServer(t *testing.T) *shutdownTestServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	active := make(chan struct{})
	s := &shutdownTestServer{
		release:         make(chan struct{}, 1),
		shutdownStarted: make(chan struct{}),
		handled:         make(chan error, 1),
		response:        make(chan shutdownResponse, 1),
	}
	s.Server = &http.Server{
		Addr: listener.Addr().String(),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(active)
			select {
			case <-s.release:
				_, err := io.WriteString(w, "finished")
				s.handled <- err
			case <-r.Context().Done():
				s.handled <- r.Context().Err()
			}
		}),
	}
	s.RegisterOnShutdown(func() { close(s.shutdownStarted) })
	served := make(chan error, 1)
	go func() { served <- s.Serve(listener) }()
	transport := &http.Transport{DisableKeepAlives: true}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
		transport.CloseIdleConnections()
		if err := awaitShutdownEvent(t, "Serve return", served); !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve returned %v, want ErrServerClosed", err)
		}
	})
	go func() {
		response, err := client.Get("http://" + s.Addr)
		result := shutdownResponse{err: err}
		if err == nil {
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			result.body, result.err = string(body), err
		}
		s.response <- result
	}()
	awaitShutdownEvent(t, "active handler", active)
	return s
}

func awaitShutdownEvent[T any](t *testing.T, name string, events <-chan T) T {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
		var zero T
		return zero
	}
}

func startTestShutdown(t *testing.T, ctx context.Context, servers ...*http.Server) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		shutdownServers(ctx, servers...)
		close(done)
	}()
	t.Cleanup(func() { awaitShutdownEvent(t, "shutdown completion", done) })
	return done
}

func TestShutdownServersDrainsBothConcurrently(t *testing.T) {
	public := newShutdownTestServer(t)
	admin := newShutdownTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startTestShutdown(t, ctx, public.Server, admin.Server)

	// Neither active handler is released until both Shutdown calls have started.
	for _, s := range []*shutdownTestServer{public, admin} {
		awaitShutdownEvent(t, "Shutdown start", s.shutdownStarted)
		conn, err := net.DialTimeout("tcp", s.Addr, time.Second)
		if err == nil {
			conn.Close()
			t.Fatal("listener still accepts connections while draining")
		}
	}
	for _, s := range []*shutdownTestServer{public, admin} {
		select {
		case <-done:
			t.Fatal("shutdown returned before all active handlers finished")
		default:
		}
		s.release <- struct{}{}
		if result := awaitShutdownEvent(t, "graceful response", s.response); result.err != nil || result.body != "finished" {
			t.Fatalf("response = %+v, want finished without error", result)
		}
		if err := awaitShutdownEvent(t, "graceful handler completion", s.handled); err != nil {
			t.Fatalf("handler did not finish gracefully: %v", err)
		}
	}
	awaitShutdownEvent(t, "shutdown completion", done)
}

func TestShutdownServersForceClosesBothAfterDeadline(t *testing.T) {
	public := newShutdownTestServer(t)
	admin := newShutdownTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	done := startTestShutdown(t, ctx, public.Server, admin.Server)

	for _, s := range []*shutdownTestServer{public, admin} {
		awaitShutdownEvent(t, "Shutdown start", s.shutdownStarted)
	}
	awaitShutdownEvent(t, "shutdown completion", done)
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("shutdown returned before the shared deadline: %v", ctx.Err())
	}
	for _, s := range []*shutdownTestServer{public, admin} {
		if err := awaitShutdownEvent(t, "force-closed handler", s.handled); !errors.Is(err, context.Canceled) {
			t.Fatalf("handler context error = %v, want canceled", err)
		}
		if result := awaitShutdownEvent(t, "force-closed response", s.response); result.err == nil {
			t.Fatalf("request was not force-closed: %+v", result)
		}
	}
}
