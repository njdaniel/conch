package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
)

// The restore host command (issue #716, docs/design/guests-and-accounts.md
// §4.2, docs/deploy.md "Restoring for real"): offline, on a data directory
// unpacked from a backup, before any conchd serves it.
//
//	conchd restore --data <dir> [--force]
//
// It makes the restored database safe to serve (store.InvalidateRestored):
// every credential, link, pending sign-in, recent-auth proof and setup token
// in it stops working, every guest visit in it ends with reason restore, and
// the TOTP steps an operator may already have used are refused. Passwords,
// authenticators, accounts, memberships and messages stay.
//
// It is an explicit command, run by the restore procedure and by nothing
// else, so an ordinary restart never does it. The procedure (conch-restore.sh,
// conch-upgrade.sh --rollback and the steps in docs/deploy.md) first puts
// the file restorePendingFile in the data directory, before it unpacks
// anything; serve refuses to start while that file is there, and this
// command removes it only after its transaction has committed. So a restore
// that fails or is interrupted at any point leaves an instance that will not
// serve, rather than one serving what the backup held: run conchd restore
// again, which is safe to repeat.
//
// A binary from before this command answers "unknown command", and the
// procedure stops there, with the file still in place.

// restorePendingFile is the name, in the data directory, of the file that
// says a restore into it has not been finished by conchd restore.
const restorePendingFile = "restore-pending"

// restorePending reports whether dataDir holds restorePendingFile. Anything
// but a clear "not there" is an error, so serve fails closed on a directory
// it cannot read.
func restorePending(dataDir string) (bool, error) {
	_, err := os.Lstat(filepath.Join(dataDir, restorePendingFile)) // #nosec G703 -- trusted operator path
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("check for an unfinished restore: %w", err)
	}
}

// refuseUnfinishedRestore is what a command that must not touch a restored
// database before conchd restore has finished says when dataDir holds one:
// serve would serve the backup's sign-ins, and a setup token made now would
// be deleted by the restore.
func refuseUnfinishedRestore(cmd, dataDir string) error {
	pending, err := restorePending(dataDir)
	if err != nil {
		return fmt.Errorf("%s: %w", cmd, err)
	}
	if pending {
		return fmt.Errorf("%s: %s was restored from a backup and the restore has not finished (%s is there); run: conchd restore --data %s",
			cmd, dataDir, restorePendingFile, dataDir)
	}
	return nil
}

func restoreUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage:
  conchd restore --data <dir> [--force]
`)
}

// runRestore serves `conchd restore`. What it did goes to stdout, as counts;
// it prints no token, code or secret, because it makes none.
func runRestore(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { restoreUsage(stderr) }
	dataDir := flags.String("data", os.Getenv("CONCHD_DATA"), "the restored data directory")
	force := flags.Bool("force", false, "run on a data directory that does not hold restore-pending")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		restoreUsage(stderr)
		return fmt.Errorf("restore: unexpected argument %q", flags.Arg(0))
	}
	if *dataDir == "" {
		return errors.New("restore: --data (or CONCHD_DATA) is required")
	}
	// Every restore procedure leaves restore-pending before it unpacks. A
	// directory without it is most likely a live one, where this would sign
	// everyone out: that takes --force.
	if !*force {
		pending, err := restorePending(*dataDir)
		if err != nil {
			return fmt.Errorf("restore: %w", err)
		}
		if !pending {
			return fmt.Errorf("restore: %s holds no %s, so no restore procedure unpacked it; nothing changed. To sign everyone out of it anyway, add --force", *dataDir, restorePendingFile)
		}
	}
	ctx := context.Background()
	st, err := openExisting(ctx, "restore", *dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	res, err := st.InvalidateRestored(ctx, time.Now())
	if err != nil {
		return fmt.Errorf("restore: nothing changed, and the data directory must not be served; run conchd restore again: %w", err)
	}
	// Only now, with the transaction committed, may the instance serve.
	marker := filepath.Join(*dataDir, restorePendingFile)
	if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) { // #nosec G703 -- trusted operator path
		return fmt.Errorf("restore: the database is safe to serve, but %s could not be removed, so serve still refuses; remove it by hand: %w", marker, err)
	}
	printRestore(stdout, res)
	return nil
}

func printRestore(w io.Writer, res store.RestoreResult) {
	_, _ = fmt.Fprintf(w, "restore: signed out %d principal(s): every credential, sign-in link, pending sign-in and re-authentication in the backup stopped working\n", len(res.Principals))
	_, _ = fmt.Fprintf(w, "restore: ended %d guest visit(s) (reason restore)\n", len(res.GuestsEnded))
	_, _ = fmt.Fprintf(w, "restore: deleted %d setup token(s)\n", res.SetupTokens)
	_, _ = fmt.Fprintln(w, "restore: invites are paused: unpause after review (conch admin invite list --state open, then conch admin invite resume)")
	_, _ = fmt.Fprintln(w, "restore: guest joining is paused: unpause after review (PUT /v1/join/settings with joining_paused false)")
	_, _ = fmt.Fprintf(w, "restore: %d webhook hook(s) are live and keep working: review them (GET /v1/hooks) and delete any that should be gone\n", res.Hooks)
	_, _ = fmt.Fprintln(w, "restore: authenticator codes are refused for about a minute after the first start")
	_, _ = fmt.Fprintln(w, "restore: done. Members sign in again with their passwords; operators with password and code, or with conchd operator recover. Agents and scripts need new credentials.")
}
