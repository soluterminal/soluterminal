package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"soluterminal-shared/machineauth"
)

// Disconnect removes this computer from its account in the cloud, not just
// locally. The token is deleted at once; what stays on disk is a revoke key
// (machineauth.RevokeKey) that can only revoke this machine, so the revoke can
// be retried while the cloud is unreachable without keeping a credential that
// still works for anything else.

const (
	cloudRevokeFile = "cloud_revoke_pending.json"
	// cloudRevokeRefusedFile records that the cloud refused a revoke (a 4xx
	// other than 404), so the app can tell the person to remove the computer
	// in Your machines. It holds a time and the cloud base only: no key, no
	// token.
	cloudRevokeRefusedFile = "cloud_revoke_refused.json"

	// cloudRevokeMaxAge drops a revoke the cloud never confirmed. By then a
	// rotated token or a manual "Remove computer" has long made it moot.
	cloudRevokeMaxAge  = 30 * 24 * time.Hour
	cloudRevokeTimeout = 10 * time.Second
	// Retry backoff for an unconfirmed revoke.
	cloudRevokeRetryMin = 30 * time.Second
	cloudRevokeRetryMax = time.Hour
)

type pendingCloudRevoke struct {
	CloudBase string `json:"cloudBase"`
	MachineID string `json:"machineId"`
	RevokeKey string `json:"revokeKey"`
	QueuedAt  string `json:"queuedAt"`
}

func cloudRevokePath(dataDir string) string {
	return filepath.Join(dataDir, cloudRevokeFile)
}

// cloudRevokeFromClaim builds the revoke record for a claim, or nil when the
// claim can't identify a machine in a cloud. fallbackRelay covers claims
// written without a relay URL.
func cloudRevokeFromClaim(c *Claim, fallbackRelay string, now time.Time) *pendingCloudRevoke {
	if c == nil || strings.TrimSpace(c.Token) == "" || strings.TrimSpace(c.MachineID) == "" {
		return nil
	}
	relay := strings.TrimSpace(c.RelayURL)
	if relay == "" {
		relay = strings.TrimSpace(fallbackRelay)
	}
	base, err := relayToHTTPBase(relay)
	if err != nil {
		return nil
	}
	return &pendingCloudRevoke{
		CloudBase: base,
		MachineID: c.MachineID,
		RevokeKey: machineauth.RevokeKey(c.Token),
		QueuedAt:  now.UTC().Format(time.RFC3339),
	}
}

// cloudRevokeQueue is the on-disk list of revokes the cloud hasn't confirmed.
// Its mutex serialises the read-modify-write between the Disconnect handler
// and the background revoker.
type cloudRevokeQueue struct {
	mu      sync.Mutex
	dataDir string
}

func (q *cloudRevokeQueue) loadLocked() []pendingCloudRevoke {
	b, err := os.ReadFile(cloudRevokePath(q.dataDir))
	if err != nil {
		return nil
	}
	var items []pendingCloudRevoke
	if json.Unmarshal(b, &items) != nil {
		return nil
	}
	return items
}

func (q *cloudRevokeQueue) saveLocked(items []pendingCloudRevoke) error {
	path := cloudRevokePath(q.dataDir)
	if len(items) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(q.dataDir, 0o700); err != nil {
		return err
	}
	return writeFileAtomicSynced(path, b, 0o600)
}

// add records a revoke, replacing any earlier one for the same machine.
func (q *cloudRevokeQueue) add(p pendingCloudRevoke) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.loadLocked()
	kept := items[:0]
	for _, it := range items {
		if it.MachineID != p.MachineID || it.CloudBase != p.CloudBase {
			kept = append(kept, it)
		}
	}
	return q.saveLocked(append(kept, p))
}

func (q *cloudRevokeQueue) list() []pendingCloudRevoke {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.loadLocked()
}

// remove drops the given revoke (matched on machine, cloud and key, so a
// newer record queued meanwhile for the same machine survives).
func (q *cloudRevokeQueue) remove(p pendingCloudRevoke) {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.loadLocked()
	kept := items[:0]
	for _, it := range items {
		if it.MachineID == p.MachineID && it.CloudBase == p.CloudBase && it.RevokeKey == p.RevokeKey {
			continue
		}
		kept = append(kept, it)
	}
	if err := q.saveLocked(kept); err != nil {
		slog.Warn("cloud revoke: could not update pending list", "err", err)
	}
}

// cloudRevoker sends queued revokes, retrying with backoff until the cloud
// confirms or the record expires. It resumes from disk after a restart.
type cloudRevoker struct {
	queue  *cloudRevokeQueue
	client *http.Client
	kick   chan struct{}
	// inflight holds the revokes being sent right now, so Disconnect's own
	// attempt and the background flush never send the same one twice (the
	// second would get 404 and read as "unlisted" after a real removal).
	inflightMu sync.Mutex
	inflight   map[pendingCloudRevoke]bool
	// retryMin and retryMax bound the backoff; tests shrink them.
	retryMin, retryMax time.Duration
}

func newCloudRevoker(dataDir string) *cloudRevoker {
	return &cloudRevoker{
		queue:  &cloudRevokeQueue{dataDir: dataDir},
		client: &http.Client{Timeout: cloudRevokeTimeout},
		kick:   make(chan struct{}, 1),

		retryMin: cloudRevokeRetryMin,
		retryMax: cloudRevokeRetryMax,
	}
}

// enqueue persists a revoke and wakes the sender. A nil record is a no-op.
func (r *cloudRevoker) enqueue(p *pendingCloudRevoke) error {
	if r == nil || p == nil {
		return nil
	}
	if err := r.queue.add(*p); err != nil {
		return err
	}
	r.wake()
	return nil
}

// wake asks the background sender to flush now.
func (r *cloudRevoker) wake() {
	if r == nil {
		return
	}
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// queued reports whether p is still waiting to be sent.
func (r *cloudRevoker) queued(p pendingCloudRevoke) bool {
	for _, it := range r.queue.list() {
		if it == p {
			return true
		}
	}
	return false
}

// claim marks p as being sent; false when it already is.
func (r *cloudRevoker) claim(p pendingCloudRevoke) bool {
	r.inflightMu.Lock()
	defer r.inflightMu.Unlock()
	if r.inflight[p] {
		return false
	}
	if r.inflight == nil {
		r.inflight = map[pendingCloudRevoke]bool{}
	}
	r.inflight[p] = true
	return true
}

func (r *cloudRevoker) release(p pendingCloudRevoke) {
	r.inflightMu.Lock()
	defer r.inflightMu.Unlock()
	delete(r.inflight, p)
}

// queueCloudRevoke persists a revoke without waking the sender: Disconnect
// makes the first attempt itself and wakes the sender only if that attempt is
// still pending. Without a revoker (tests) the record is written for the next
// run to send.
func (a *apiServer) queueCloudRevoke(p *pendingCloudRevoke) error {
	if p == nil {
		return nil
	}
	if a.cloudRevoker != nil {
		return a.cloudRevoker.queue.add(*p)
	}
	return (&cloudRevokeQueue{dataDir: a.dataDir}).add(*p)
}

func (r *cloudRevoker) run(ctx context.Context) {
	delay := r.retryMin
	for {
		if r.flush(ctx) {
			delay = r.retryMin
		} else {
			delay = min(delay*2, r.retryMax)
		}
		var wait <-chan time.Time
		if len(r.queue.list()) > 0 {
			wait = time.After(delay)
		}
		select {
		case <-ctx.Done():
			return
		case <-r.kick:
			delay = r.retryMin
		case <-wait:
		}
	}
}

// cloudRevokeDisconnectWait bounds how long Disconnect waits for the first
// send, so it can say what happened. The token is already deleted by then.
// A var so tests can shorten it.
var cloudRevokeDisconnectWait = 3 * time.Second

// cloudRevokeOutcome is what a revoke came to, as Disconnect reports it.
type cloudRevokeOutcome string

const (
	// revokeRemoved: the cloud removed the computer (2xx).
	revokeRemoved cloudRevokeOutcome = "removed"
	// revokeUnlisted: the cloud answered 404, which by design means either
	// "no such computer" or "wrong key" (so the endpoint can't be used to
	// probe machine ids). Done, but never called a removal.
	revokeUnlisted cloudRevokeOutcome = "unlisted"
	// revokeRefused: any other 4xx. The cloud won't accept this request, so
	// it is dropped and the person is told to remove the computer by hand.
	revokeRefused cloudRevokeOutcome = "refused"
	// revokePending: no answer yet (network error, 429, 5xx); retried in the
	// background.
	revokePending cloudRevokeOutcome = "pending"
)

// flush tries every pending revoke once. It reports whether the queue is now
// empty.
func (r *cloudRevoker) flush(ctx context.Context) bool {
	now := time.Now()
	for _, p := range r.queue.list() {
		if queued, err := time.Parse(time.RFC3339, p.QueuedAt); err == nil && now.Sub(queued) > cloudRevokeMaxAge {
			slog.Warn("cloud revoke: giving up after 30 days", "machine", p.MachineID)
			r.queue.remove(p)
			continue
		}
		if !r.claim(p) {
			continue // Disconnect is sending it right now
		}
		if r.queued(p) { // not settled by Disconnect since the list was read
			r.settle(p, r.send(ctx, p))
		}
		r.release(p)
	}
	return len(r.queue.list()) == 0
}

// attempt sends one revoke now, for Disconnect, and settles it like the
// background sender would. A pending outcome stays queued for retry.
func (r *cloudRevoker) attempt(ctx context.Context, p pendingCloudRevoke) cloudRevokeOutcome {
	if !r.claim(p) {
		return revokePending // the background sender has it; it will settle it
	}
	defer r.release(p)
	if !r.queued(p) {
		return revokePending // the background sender already settled it
	}
	outcome := r.send(ctx, p)
	r.settle(p, outcome)
	if outcome == revokePending {
		r.wake()
	}
	return outcome
}

// settle removes a revoke that is done and records a refusal.
func (r *cloudRevoker) settle(p pendingCloudRevoke, outcome cloudRevokeOutcome) {
	switch outcome {
	case revokePending:
		return
	case revokeRefused:
		if err := writeCloudRevokeRefused(r.queue.dataDir, p.CloudBase, time.Now()); err != nil {
			slog.Warn("cloud revoke: could not record the refusal", "err", err)
		}
	}
	r.queue.remove(p)
}

// send posts one revoke and classifies the answer.
func (r *cloudRevoker) send(ctx context.Context, p pendingCloudRevoke) cloudRevokeOutcome {
	body, _ := json.Marshal(map[string]string{"machineId": p.MachineID, "revokeKey": p.RevokeKey})
	ctx, cancel := context.WithTimeout(ctx, cloudRevokeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.CloudBase, "/")+machineauth.RevokePath, bytes.NewReader(body))
	if err != nil {
		// A malformed record can never be sent: the person has to remove
		// the computer by hand.
		slog.Warn("cloud revoke: unusable record; remove this computer in Your machines", "machine", p.MachineID)
		return revokeRefused
	}
	req.Header.Set("Content-Type", machineauth.ContentTypeJSON)
	resp, err := r.client.Do(req)
	if err != nil {
		slog.Info("cloud revoke: will retry", "machine", p.MachineID, "err", err)
		return revokePending
	}
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		slog.Info("cloud revoke: computer removed from its account", "machine", p.MachineID)
		return revokeRemoved
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		slog.Info("cloud revoke: will retry", "machine", p.MachineID, "http", resp.StatusCode)
		return revokePending
	case resp.StatusCode == http.StatusNotFound:
		slog.Info("cloud revoke: account no longer lists this computer, or didn't accept the key", "machine", p.MachineID)
		return revokeUnlisted
	default:
		slog.Warn("cloud revoke: the cloud refused to remove this computer; remove it in Your machines", "machine", p.MachineID, "http", resp.StatusCode)
		return revokeRefused
	}
}

// cloudRevokeRefusedRecord is what cloud_revoke_refused.json holds.
type cloudRevokeRefusedRecord struct {
	CloudBase string `json:"cloudBase"`
	At        string `json:"at"`
}

func writeCloudRevokeRefused(dataDir, cloudBase string, at time.Time) error {
	b, err := json.Marshal(cloudRevokeRefusedRecord{CloudBase: cloudBase, At: at.UTC().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	return writeFileAtomicSynced(filepath.Join(dataDir, cloudRevokeRefusedFile), b, 0o600)
}

func readCloudRevokeRefused(dataDir string) *cloudRevokeRefusedRecord {
	b, err := os.ReadFile(filepath.Join(dataDir, cloudRevokeRefusedFile))
	if err != nil {
		return nil
	}
	var rec cloudRevokeRefusedRecord
	if json.Unmarshal(b, &rec) != nil || rec.At == "" {
		return nil
	}
	return &rec
}

func clearCloudRevokeRefused(dataDir string) {
	if err := os.Remove(filepath.Join(dataDir, cloudRevokeRefusedFile)); err != nil && !os.IsNotExist(err) {
		slog.Warn("cloud revoke: could not clear the refusal record", "err", err)
	}
}

// cloudMachinesURL is where a person removes a computer by hand: the account
// page of the cloud the revoke was sent to.
func cloudMachinesURL(cloudBase string) string {
	if cloudBase == "" {
		return ""
	}
	return strings.TrimRight(cloudBase, "/") + "/"
}
