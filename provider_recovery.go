package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"
)

func queueProviderRecovery(db *sql.DB, id string, limit *providerLimit) error {
	_, err := db.Exec(`INSERT INTO provider_recoveries VALUES (?,?,'waiting','Waiting for provider availability')
 ON CONFLICT(issue_id) DO UPDATE SET retry_at=excluded.retry_at,state=excluded.state,reason=excluded.reason`, id, limit.RetryAt.Unix())
	return err
}

func stopProviderRecovery(db *sql.DB, id, reason string) error {
	_, err := db.Exec("UPDATE provider_recoveries SET state='manual',reason=? WHERE issue_id=?", reason, id)
	return err
}

func recordedProvider(c *config, saved runContext) config {
	resumed := *c
	resumed.Engine, resumed.Model = saved.Engine, saved.Model
	resumed.ReasoningEffort = saved.ReasoningEffort
	resumed.Codex, resumed.OpenCode = saved.Executable, saved.Executable
	return resumed
}

// The worker lock covers selection, checkpointing, probing and continuation.
func (r *runner) recoverProvider(c *config, runs string, db *sql.DB, lock *os.File) (handled, deferred bool, runErr error) {
	var id string
	var retryAt int64
	err := db.QueryRow("SELECT issue_id,retry_at FROM provider_recoveries WHERE state='waiting' ORDER BY retry_at,issue_id LIMIT 1").Scan(&id, &retryAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return true, false, err
	}
	if retryAt > time.Now().Unix() {
		fmt.Fprintf(r.out, "%s: waiting for provider; next probe after %s\n", id, time.Unix(retryAt, 0).UTC().Format(time.RFC3339))
		return true, true, nil
	}
	if err = r.checkCapacity(c); err != nil {
		fmt.Fprintln(r.out, err.Error())
		return true, true, nil
	}
	// A crash after this checkpoint requires inspection, never a replay.
	if err = stopProviderRecovery(db, id, "Automatic recovery interrupted or failed; inspect cron.log and retained checkout before manual resume/reconcile"); err != nil {
		return true, false, err
	}
	status, err := claimedStatus(db, id)
	if err != nil {
		return true, false, err
	}
	if status != "blocked" {
		return true, false, errors.New("provider recovery requires the recorded blocked run; inspect before continuing")
	}
	saved, err := loadRecovery(c, id, runs, db)
	if err != nil {
		return true, false, err
	}
	if saved.Engine != "codex" && saved.Engine != "opencode" || saved.Executable == "" {
		return true, false, errors.New("invalid recorded agent")
	}
	resumed := recordedProvider(c, saved)
	_, err = r.checkProvider(&resumed, saved.Checkout, runs, db, lock)
	if err == nil {
		fmt.Fprintf(r.out, "%s: provider available; automatically resuming recorded session\n", textAt(saved.Issue, "identifier"))
		err = r.resume(c, id, runs, db, lock)
	}
	var limit *providerLimit
	var capacity *capacityWait
	if errors.As(err, &capacity) {
		// Space may fall during the provider probe. No continuation has started;
		// retain its original eligibility and retry time for a later poll.
		if _, e := db.Exec("UPDATE provider_recoveries SET state='waiting',reason='Waiting for disk capacity' WHERE issue_id=?", id); e != nil {
			return true, false, errors.Join(err, e)
		}
		fmt.Fprintln(r.out, capacity.Error())
		return true, true, nil
	}
	if errors.As(err, &limit) && !limit.recordingFailed {
		if e := saveCooldown(db, &resumed, limit); e != nil {
			return true, false, errors.Join(err, e)
		}
		if e := queueProviderRecovery(db, id, limit); e != nil {
			return true, false, errors.Join(err, e)
		}
		fmt.Fprintln(r.out, limit.Error())
		return true, true, nil
	}
	return true, false, err
}
