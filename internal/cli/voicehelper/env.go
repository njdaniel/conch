package voicehelper

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"github.com/njdaniel/conch/pkg/schema"
)

const (
	configDirEnv = "CONCH_CONFIG_DIR"
	tokenEnv     = "CONCH_TOKEN"
)

// scrubbedEnv are the variables, besides every CONCH_ one, that the helper
// never inherits: they would change what it logs, and a backtrace can print
// what a log never would.
var scrubbedEnv = []string{"RUST_LOG", "RUST_BACKTRACE", "RUST_LIB_BACKTRACE"}

// helperEnv is the helper's environment (design note §9): the owner's own,
// without any CONCH_ variable or the Rust logging ones, and then the
// configuration directory and, only when the owner's token came from
// CONCH_TOKEN, that token. Nothing else is added.
func helperEnv(parent []string, p Profile) []string {
	env := make([]string, 0, len(parent)+2)
	for _, kv := range parent {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" || strings.HasPrefix(name, "CONCH_") || slices.Contains(scrubbedEnv, name) {
			continue
		}
		env = append(env, kv)
	}
	if p.ConfigDir != "" {
		env = append(env, configDirEnv+"="+p.ConfigDir)
	}
	if p.Credential == schema.VoiceControlCredentialEnv {
		env = append(env, tokenEnv+"="+p.Token)
	}
	return env
}

// check refuses a profile no helper could be started for. Its errors never
// repeat the token.
func (p Profile) check() error {
	switch p.Credential {
	case schema.VoiceControlCredentialStored:
	case schema.VoiceControlCredentialEnv:
		if strings.TrimSpace(p.Token) == "" || strings.ContainsRune(p.Token, 0) {
			return ErrNoCredential
		}
	default:
		return ErrNoCredential
	}
	if p.ConfigDir != "" && (!filepath.IsAbs(p.ConfigDir) || strings.ContainsRune(p.ConfigDir, 0)) {
		return errors.New("voicehelper: the configuration directory must be an absolute path")
	}
	return nil
}
