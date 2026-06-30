package ipfsclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBlockGetLocalReturnsBytesAndForcesOffline(t *testing.T) {
	var gotOffline string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/block/get" {
			t.Fatalf("path %s", r.URL.Path)
		}
		gotOffline = r.URL.Query().Get("offline")
		w.Write([]byte("BLOCKBYTES"))
	}))
	defer srv.Close()
	c := New(srv.URL)
	got, err := c.BlockGetLocal(context.Background(), "bafkreiabc")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "BLOCKBYTES" {
		t.Fatalf("got %q", got)
	}
	if gotOffline != "true" {
		t.Fatalf("offline=%q, want true (no Bitswap)", gotOffline)
	}
}

func TestBlockGetLocalNotPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // Kubo returns 500 for a missing block
	}))
	defer srv.Close()
	if _, err := New(srv.URL).BlockGetLocal(context.Background(), "bafkreimissing"); !errors.Is(err, ErrBlockNotLocal) {
		t.Fatalf("want ErrBlockNotLocal, got %v", err)
	}
}
