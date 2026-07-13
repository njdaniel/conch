package main

import "testing"

func TestRunVersion(t *testing.T) {
	if err := run([]string{"version"}); err != nil {
		t.Fatalf("run version: %v", err)
	}
}

func TestRunNoCommand(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("run with no command: want error, got nil")
	}
}

func TestRunUnknownCommand(t *testing.T) {
	if err := run([]string{"frobnicate"}); err == nil {
		t.Fatal("run unknown command: want error, got nil")
	}
}

func TestRunServeRequiresData(t *testing.T) {
	t.Setenv("CONCHD_DATA", "")
	if err := runServe([]string{"--listen", "127.0.0.1:0"}); err == nil {
		t.Fatal("runServe without --data: want error, got nil")
	}
}
