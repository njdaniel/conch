package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --server-name (issue #710): the front door's label. Unset is the server's
// default; a name the schema would not show is refused at start.
func TestParseServeArgsServerName(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     string
		want    string
		wantErr bool
	}{
		{"default", nil, "", "", false},
		{"flag", []string{"--server-name", "Harbour Club"}, "", "Harbour Club", false},
		{"env", nil, "conch.example", "conch.example", false},
		{"flag beats env", []string{"--server-name=Harbour"}, "conch.example", "Harbour", false},
		{"blank", []string{"--server-name", "   "}, "", "", true},
		{"too long", []string{"--server-name", strings.Repeat("x", 65)}, "", "", true},
		{"a control character", []string{"--server-name", "a\tb"}, "", "", true},
		{"the hidden namespace", []string{"--server-name", "hidden-3"}, "", "", true},
		{"the guest namespace, from the environment", nil, "Guest-x", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONCHD_SERVER_NAME", tt.env)
			opts, err := parseServeArgs(append([]string{"--data", t.TempDir()}, tt.args...))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && opts.serverName != tt.want {
				t.Errorf("serverName = %q, want %q", opts.serverName, tt.want)
			}
		})
	}
}

// The offline bootstraps reserve the server's name as serve does (issue
// #798): --server-name, else CONCHD_SERVER_NAME, else conch. An operator
// named as the server, in any spelling that looks the same, is refused and
// nothing is written; a name that is another server's is anyone's. A server
// name serve would refuse is refused here too, before the data directory is
// touched.
func TestOfflineBootstrapReservesServerName(t *testing.T) {
	commands := []struct {
		name string
		run  func(dir, operator string, flags []string) (string, error)
	}{
		{"bootstrap-operator", func(dir, operator string, flags []string) (string, error) {
			var out, errOut bytes.Buffer
			err := runBootstrapOperator(append([]string{"--data", dir, "--name", operator}, flags...), &out, &errOut)
			return out.String(), err
		}},
		{"operator bootstrap", func(dir, operator string, flags []string) (string, error) {
			out, _, err := runOp(t, append(append([]string{"bootstrap", "--data", dir}, flags...), operator)...)
			return out, err
		}},
	}
	tests := []struct {
		name     string
		env      string
		flags    []string
		operator string
		// want is what a refusal says; empty when the bootstrap succeeds.
		want string
		// noDir is whether a refusal leaves the data directory unmade.
		noDir bool
	}{
		{"the default name", "", nil, "conch", `the server's, "conch"`, false},
		{"the default name in capitals", "", nil, "CONCH", `the server's, "conch"`, false},
		{"the default name with a Cyrillic о", "", nil, "cоnch", `the server's, "conch"`, false},
		{"another name", "", nil, "root", "", false},
		{"the configured name", "", []string{"--server-name", "Harbour"}, "Harbour", `the server's, "Harbour"`, false},
		{"the default name, another configured", "", []string{"--server-name", "Harbour"}, "conch", "", false},
		{"the name from the environment", "Harbour", nil, "harbour", `the server's, "Harbour"`, false},
		{"the flag beats the environment", "Harbour", []string{"--server-name=Lagoon"}, "Harbour", "", false},
		{"a server name in the hidden namespace", "", []string{"--server-name", "hidden-1"}, "root", "--server-name: server name: names beginning with hidden-", true},
		{"a server name in the guest namespace", "guest house", nil, "root", "--server-name: server name: names beginning with guest", true},
		{"a blank server name", "", []string{"--server-name", "  "}, "root", "--server-name: server name must not be blank", true},
	}
	for _, cmd := range commands {
		for _, tt := range tests {
			t.Run(cmd.name+"/"+tt.name, func(t *testing.T) {
				t.Setenv("CONCHD_DATA", "")
				t.Setenv("CONCHD_SERVER_NAME", tt.env)
				dir := filepath.Join(t.TempDir(), "data") // does not exist yet
				out, err := cmd.run(dir, tt.operator, tt.flags)
				if tt.want == "" {
					if err != nil || out == "" {
						t.Fatalf("err = %v, stdout = %q; want the operator made", err, out)
					}
					if got := dbCounts(t, dir)[2]; got != 1 {
						t.Errorf("%d principals, want the operator", got)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), tt.want) || out != "" {
					t.Fatalf("err = %v, stdout = %q; want a refusal saying %q", err, out, tt.want)
				}
				if _, statErr := os.Stat(dir); tt.noDir {
					if !errors.Is(statErr, os.ErrNotExist) {
						t.Errorf("the data directory exists after the refusal (%v)", statErr)
					}
				} else if got := dbCounts(t, dir); got != [3]int{} {
					t.Errorf("database after the refusal = %v, want empty", got)
				}
			})
		}
	}
}
