package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/money"
)

// Postgres is the Ledger backed by the schema in migrations/0001_ledger.sql.
//
// Lock order is always hold row, then account row, so settles, releases and
// the reconciler cannot deadlock. Reserve and Deposit only touch the account.
type Postgres struct {
	pool *pgxpool.Pool
	now  func() time.Time

	lookupTimeout time.Duration // bound on one UsageLookup call during Reconcile
}

const defaultLookupTimeout = 10 * time.Second

var _ Ledger = (*Postgres)(nil)

func New(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool, now: time.Now, lookupTimeout: defaultLookupTimeout}
}

// WithClock returns a copy that stamps expires_at from now. Tests use it to
// move time; Reconcile takes its own `now` argument.
func (p *Postgres) WithClock(now func() time.Time) *Postgres {
	c := *p
	c.now = now
	return &c
}

// WithLookupTimeout returns a copy with a different per-lookup bound; tests
// shrink it.
func (p *Postgres) WithLookupTimeout(d time.Duration) *Postgres {
	c := *p
	c.lookupTimeout = d
	return &c
}

const uniqueViolation = "23505"

func isUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == uniqueViolation
}

func (p *Postgres) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, p.pool, fn)
}

func (p *Postgres) CreateAccount(ctx context.Context, name string) (string, error) {
	var id string
	err := p.pool.QueryRow(ctx, `INSERT INTO accounts (name) VALUES ($1) RETURNING id`, name).Scan(&id)
	return id, err
}

// insertEntry appends one row to the history that Audit later recomputes I1
// and I2 from. Every mutation calls it inside the same transaction as the
// balance change.
func insertEntry(ctx context.Context, tx pgx.Tx, accountID string, holdID *string, kind string,
	amount, balanceAfter, heldAfter money.Nanos, idemKey, memo string) error {
	_, err := tx.Exec(ctx, `INSERT INTO ledger_entries
		(account_id, hold_id, kind, amount_nanos, balance_after, held_after, idem_key, memo)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		accountID, holdID, kind, int64(amount), int64(balanceAfter), int64(heldAfter), idemKey, memo)
	return err
}

func (p *Postgres) Deposit(ctx context.Context, accountID string, amount money.Nanos, idemKey, memo string) error {
	if accountID == "" || idemKey == "" || amount <= 0 {
		return fmt.Errorf("%w: deposit needs account, key and positive amount", ErrInvalidRequest)
	}
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		var bal, held money.Nanos
		err := tx.QueryRow(ctx, `UPDATE accounts SET balance_nanos = balance_nanos + $2
			WHERE id = $1 RETURNING balance_nanos, held_nanos`, accountID, int64(amount)).Scan(&bal, &held)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAccountNotFound
		}
		if err != nil {
			return err
		}
		return insertEntry(ctx, tx, accountID, nil, "deposit", amount, bal, held, idemKey, memo)
	})
	if isUnique(err) {
		// I4: the key was used before (or a duplicate just committed). The
		// whole tx rolled back, so the balance is untouched; compare and replay.
		var prev money.Nanos
		qerr := p.pool.QueryRow(ctx, `SELECT amount_nanos FROM ledger_entries
			WHERE account_id=$1 AND idem_key=$2 AND kind='deposit'`, accountID, idemKey).Scan(&prev)
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrIdempotencyConflict // key taken by a different entry kind
			}
			return qerr
		}
		if prev != amount {
			return ErrIdempotencyConflict
		}
		return nil
	}
	return err
}

func (p *Postgres) Reserve(ctx context.Context, r ReserveReq) (Hold, error) {
	switch {
	case r.AccountID == "" || r.IdemKey == "" || r.Amount <= 0 || r.TTL <= 0:
		return Hold{}, fmt.Errorf("%w: reserve needs account, key, positive amount and ttl", ErrInvalidRequest)
	case r.Kind != KindModel && r.Kind != KindTool && r.Kind != KindPayment:
		return Hold{}, fmt.Errorf("%w: unknown kind %q", ErrInvalidRequest, r.Kind)
	}
	var h Hold
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		// Insert first, ON CONFLICT DO NOTHING: a concurrent duplicate waits
		// here for the winner to commit, then falls into the replay branch (I4).
		expires := p.now().Add(r.TTL).UTC()
		var id string
		err := tx.QueryRow(ctx, `INSERT INTO holds (account_id, idem_key, kind, amount_nanos, expires_at)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (account_id, idem_key) DO NOTHING RETURNING id`,
			r.AccountID, r.IdemKey, string(r.Kind), int64(r.Amount), expires).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			prev, err := scanHold(tx.QueryRow(ctx, holdSelect+` WHERE account_id=$1 AND idem_key=$2`, r.AccountID, r.IdemKey))
			if err != nil {
				return err
			}
			if prev.Amount != r.Amount || prev.Kind != r.Kind {
				return ErrIdempotencyConflict
			}
			h = prev.toHold()
			return nil
		}
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23503" { // foreign_key_violation
			return ErrAccountNotFound
		}
		if err != nil {
			return err
		}

		// I1/I5: the budget check IS the update. No read-then-write, so two
		// reserves cannot both see the same free balance.
		var bal, held money.Nanos
		err = tx.QueryRow(ctx, `UPDATE accounts SET held_nanos = held_nanos + $2
			WHERE id = $1 AND status = 'active' AND balance_nanos - held_nanos >= $2
			RETURNING balance_nanos, held_nanos`, r.AccountID, int64(r.Amount)).Scan(&bal, &held)
		if errors.Is(err, pgx.ErrNoRows) {
			var status string
			if qerr := tx.QueryRow(ctx, `SELECT status FROM accounts WHERE id=$1`, r.AccountID).Scan(&status); qerr != nil {
				return qerr
			}
			if status != "active" {
				return ErrAccountInactive
			}
			return ErrInsufficientFunds // rollback also discards the hold insert
		}
		if err != nil {
			return err
		}
		if err := insertEntry(ctx, tx, r.AccountID, &id, "hold", r.Amount, bal, held, r.IdemKey, r.Memo); err != nil {
			return err
		}
		h = Hold{ID: id, AccountID: r.AccountID, IdemKey: r.IdemKey, Kind: r.Kind, Amount: r.Amount,
			Status: "active", ExpiresAt: expires}
		return nil
	})
	if err != nil {
		return Hold{}, err
	}
	return h, nil
}

// holdRow is a holds row as read under lock.
type holdRow struct {
	ID, AccountID, IdemKey, Provider, UpstreamID, Status string
	Kind                                                 Kind
	Amount, Settled, Unrecovered                         money.Nanos
	ExpiresAt                                            time.Time
}

func (h holdRow) toHold() Hold {
	return Hold{ID: h.ID, AccountID: h.AccountID, IdemKey: h.IdemKey, Kind: h.Kind, Amount: h.Amount,
		Status: h.Status, ExpiresAt: h.ExpiresAt, Provider: h.Provider, UpstreamID: h.UpstreamID}
}

const holdSelect = `SELECT id, account_id, idem_key, kind, amount_nanos, status, expires_at,
	provider, upstream_id, settled_nanos, unrecovered_nanos FROM holds`

func scanHold(row pgx.Row) (holdRow, error) {
	var h holdRow
	var kind string
	err := row.Scan(&h.ID, &h.AccountID, &h.IdemKey, &kind, &h.Amount, &h.Status, &h.ExpiresAt,
		&h.Provider, &h.UpstreamID, &h.Settled, &h.Unrecovered)
	h.Kind = Kind(kind)
	return h, err
}

func lockHold(ctx context.Context, tx pgx.Tx, holdID string) (holdRow, error) {
	h, err := scanHold(tx.QueryRow(ctx, holdSelect+` WHERE id=$1 FOR UPDATE`, holdID))
	if errors.Is(err, pgx.ErrNoRows) {
		return h, ErrHoldNotFound
	}
	return h, err
}

func (p *Postgres) Extend(ctx context.Context, holdID string, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("%w: ttl must be positive", ErrInvalidRequest)
	}
	// GREATEST: a late, shorter extend must never shorten the lease.
	tag, err := p.pool.Exec(ctx, `UPDATE holds SET expires_at = GREATEST(expires_at, $2)
		WHERE id=$1 AND status='active'`, holdID, p.now().Add(ttl).UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return p.missingOrInactive(ctx, holdID)
	}
	return nil
}

func (p *Postgres) missingOrInactive(ctx context.Context, holdID string) error {
	var one int
	err := p.pool.QueryRow(ctx, `SELECT 1 FROM holds WHERE id=$1`, holdID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrHoldNotFound
	}
	if err != nil {
		return err
	}
	return ErrHoldNotActive
}

func (p *Postgres) AttachUpstream(ctx context.Context, holdID, provider, upstreamID string) error {
	tag, err := p.pool.Exec(ctx, `UPDATE holds SET provider=$2, upstream_id=$3
		WHERE id=$1 AND status='active'`, holdID, provider, upstreamID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return p.missingOrInactive(ctx, holdID)
	}
	return nil
}

// charge applies I7: the most a settle may take is the hold plus whatever
// balance is free; the rest is recorded, never taken (balance stays >= 0).
func charge(actual, hold, free money.Nanos) (charged, unrecovered money.Nanos) {
	if limit := hold + free; actual > limit {
		return limit, actual - limit
	}
	return actual, 0
}

// settleLocked finishes an active, locked hold. Charged and released are
// distinct entries. A settle entry moves balance by -charged and held by
// -min(charged, hold); a release entry returns the unspent rest of the hold.
// Audit relies on that split.
func settleLocked(ctx context.Context, tx pgx.Tx, h holdRow, actual money.Nanos, key, memo string) (Settlement, error) {
	var bal, held money.Nanos
	if err := tx.QueryRow(ctx, `SELECT balance_nanos, held_nanos FROM accounts WHERE id=$1 FOR UPDATE`,
		h.AccountID).Scan(&bal, &held); err != nil {
		return Settlement{}, err
	}
	charged, unrec := charge(actual, h.Amount, bal-held) // I7
	heldPart := min(charged, h.Amount)
	released := h.Amount - heldPart
	newBal, midHeld := bal-charged, held-heldPart
	newHeld := midHeld - released

	if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_nanos=$2, held_nanos=$3 WHERE id=$1`,
		h.AccountID, int64(newBal), int64(newHeld)); err != nil {
		return Settlement{}, err
	}
	// I3: the status flips exactly once; the caller holds the row lock.
	if _, err := tx.Exec(ctx, `UPDATE holds SET status='settled', settled_nanos=$2, unrecovered_nanos=$3 WHERE id=$1`,
		h.ID, int64(charged), int64(unrec)); err != nil {
		return Settlement{}, err
	}
	if unrec > 0 {
		memo = fmt.Sprintf("%s [unrecovered=%d]", memo, unrec) // flags the audit row (I7)
	}
	if err := insertEntry(ctx, tx, h.AccountID, &h.ID, "settle", charged, newBal, midHeld, key, memo); err != nil {
		return Settlement{}, err
	}
	if released > 0 {
		if err := insertEntry(ctx, tx, h.AccountID, &h.ID, "release", released, newBal, newHeld, key, memo); err != nil {
			return Settlement{}, err
		}
	}
	return Settlement{HoldID: h.ID, Charged: charged, Released: released, Unrecovered: unrec}, nil
}

func releaseLocked(ctx context.Context, tx pgx.Tx, h holdRow, key, reason string) (Settlement, error) {
	var bal, held money.Nanos
	if err := tx.QueryRow(ctx, `UPDATE accounts SET held_nanos = held_nanos - $2 WHERE id=$1
		RETURNING balance_nanos, held_nanos`, h.AccountID, int64(h.Amount)).Scan(&bal, &held); err != nil {
		return Settlement{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE holds SET status='released' WHERE id=$1`, h.ID); err != nil {
		return Settlement{}, err
	}
	if err := insertEntry(ctx, tx, h.AccountID, &h.ID, "release", h.Amount, bal, held, key, reason); err != nil {
		return Settlement{}, err
	}
	return Settlement{HoldID: h.ID, Released: h.Amount}, nil
}

// entryHold reports which hold a (key, kind) entry belongs to, if any.
func entryHold(ctx context.Context, tx pgx.Tx, accountID, key, kind string) (holdID string, found bool, err error) {
	var id *string
	err = tx.QueryRow(ctx, `SELECT hold_id FROM ledger_entries WHERE account_id=$1 AND idem_key=$2 AND kind=$3`,
		accountID, key, kind).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if id != nil {
		holdID = *id
	}
	return holdID, err == nil, err
}

func (p *Postgres) Settle(ctx context.Context, r SettleReq) (Settlement, error) {
	if r.HoldID == "" || r.IdemKey == "" || r.Actual < 0 {
		return Settlement{}, fmt.Errorf("%w: settle needs hold, key and non-negative actual", ErrInvalidRequest)
	}
	var s Settlement
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		h, err := lockHold(ctx, tx, r.HoldID)
		if err != nil {
			return err
		}
		switch h.Status {
		case "active":
			s, err = settleLocked(ctx, tx, h, r.Actual, r.IdemKey, r.Memo)
			if isUnique(err) {
				return ErrIdempotencyConflict // key already used for another hold
			}
			return err
		case "settled":
			// I3/I4: replay only if this exact key produced the settlement.
			owner, found, err := entryHold(ctx, tx, h.AccountID, r.IdemKey, "settle")
			if err != nil {
				return err
			}
			if !found || owner != h.ID {
				return ErrHoldNotActive
			}
			if h.Settled+h.Unrecovered != r.Actual { // actual = charged + unrecovered
				return ErrIdempotencyConflict
			}
			s = Settlement{HoldID: h.ID, Charged: h.Settled, Released: h.Amount - min(h.Settled, h.Amount), Unrecovered: h.Unrecovered}
			return nil
		default:
			return ErrHoldNotActive
		}
	})
	return s, err
}

func (p *Postgres) Release(ctx context.Context, holdID, idemKey, reason string) (Settlement, error) {
	if holdID == "" || idemKey == "" {
		return Settlement{}, fmt.Errorf("%w: release needs hold and key", ErrInvalidRequest)
	}
	var s Settlement
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		h, err := lockHold(ctx, tx, holdID)
		if err != nil {
			return err
		}
		switch h.Status {
		case "active":
			s, err = releaseLocked(ctx, tx, h, idemKey, reason)
			if isUnique(err) {
				return ErrIdempotencyConflict
			}
			return err
		case "released":
			owner, found, err := entryHold(ctx, tx, h.AccountID, idemKey, "release")
			if err != nil {
				return err
			}
			if !found || owner != h.ID {
				return ErrHoldNotActive
			}
			s = Settlement{HoldID: h.ID, Released: h.Amount}
			return nil
		default:
			return ErrHoldNotActive
		}
	})
	return s, err
}

func (p *Postgres) Balance(ctx context.Context, accountID string) (Balance, error) {
	b := Balance{AccountID: accountID}
	err := p.pool.QueryRow(ctx, `SELECT balance_nanos, held_nanos FROM accounts WHERE id=$1`, accountID).Scan(&b.Balance, &b.Held)
	if errors.Is(err, pgx.ErrNoRows) {
		return Balance{}, ErrAccountNotFound
	}
	return b, err
}

// Reconcile finds holds past expires_at and settles or releases them (I6). It
// lives on Postgres, not the Ledger interface: scanning expired holds with
// FOR UPDATE SKIP LOCKED needs a transaction the interface cannot express.
//
// Each hold gets its own short transaction, so one bad hold cannot block the
// rest. UsageLookup is a network call, so it runs before the transaction, with
// no lock and no connection held, under lookupTimeout; the transaction then
// re-checks that the hold is still active, expired and unchanged.
// Keys are "reconcile:<hold id>" so a repeat pass or a second reconciler can
// never produce a second terminal entry (I3, I4). A hold a live settle holds
// locked is skipped by SKIP LOCKED; one that went terminal first is no longer
// selected. Both count nowhere.
func (p *Postgres) Reconcile(ctx context.Context, lookup UsageLookup, now time.Time) (Report, error) {
	rows, err := p.pool.Query(ctx, `SELECT id FROM holds WHERE status='active' AND expires_at <= $1
		ORDER BY expires_at, id LIMIT 10000`, now.UTC())
	if err != nil {
		return Report{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Report{}, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Report{}, err
	}

	var rep Report
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		settled, err := p.reconcileOne(ctx, lookup, id, now)
		switch {
		case err == nil && settled == "settled":
			rep.Settled++
		case err == nil && settled == "released":
			rep.Released++
		case err == nil, errors.Is(err, ErrHoldNotActive), errors.Is(err, pgx.ErrNoRows):
			// lost the race to a live settle: nothing to count
		default:
			rep.Errors++
		}
	}
	return rep, nil
}

func (p *Postgres) reconcileOne(ctx context.Context, lookup UsageLookup, id string, now time.Time) (outcome string, err error) {
	// 1. Unlocked peek: is it still expired and active, and where is its usage?
	peek, err := scanHold(p.pool.QueryRow(ctx, holdSelect+` WHERE id=$1 AND status='active' AND expires_at <= $2`, id, now.UTC()))
	if err != nil {
		return "", err // ErrNoRows: went terminal or was extended since the scan
	}

	// 2. The (possibly slow, networked) lookup runs with no transaction, no row
	// lock and no pool connection held, so it can never stall a live Settle.
	var u Usage
	var found bool
	if peek.UpstreamID != "" { // never attached: nothing to look up
		lctx, cancel := context.WithTimeout(ctx, p.lookupTimeout)
		u, found, err = lookup.Lookup(lctx, peek.Provider, peek.UpstreamID)
		cancel()
		if err != nil {
			return "", err
		}
	}

	// 3. Lock and re-check: anything that changed while we looked (settled,
	// released, extended, re-attached) means our answer may be stale; skip.
	key := "reconcile:" + peek.ID
	err = p.inTx(ctx, func(tx pgx.Tx) error {
		h, err := scanHold(tx.QueryRow(ctx, holdSelect+` WHERE id=$1 AND status='active' AND expires_at <= $2
			AND provider=$3 AND upstream_id=$4 FOR UPDATE SKIP LOCKED`, id, now.UTC(), peek.Provider, peek.UpstreamID))
		if err != nil {
			return err // ErrNoRows: locked elsewhere or changed
		}
		if found && u.Cost >= 0 {
			if _, err := settleLocked(ctx, tx, h, u.Cost, key, "reconciled from usage: "+u.Source); err != nil {
				return err
			}
			outcome = "settled"
			return nil
		}
		if _, err := releaseLocked(ctx, tx, h, key, "reconciler: unmetered"); err != nil {
			return err
		}
		outcome = "released"
		return nil
	})
	return outcome, err
}

// Audit recomputes I1 and I2 from ledger_entries and returns a descriptive
// error on the first mismatch. One repeatable-read snapshot, so concurrent
// writes cannot fake a mismatch.
func (p *Postgres) Audit(ctx context.Context) error {
	return pgx.BeginTxFunc(ctx, p.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var id string
		var a, b, c, d money.Nanos

		// I1: solvency, in the accounts and at every point of history.
		err := tx.QueryRow(ctx, `SELECT id, balance_nanos, held_nanos FROM accounts
			WHERE held_nanos < 0 OR balance_nanos < held_nanos ORDER BY id LIMIT 1`).Scan(&id, &a, &b)
		if err == nil {
			return fmt.Errorf("audit I1: account %s balance=%d held=%d violates 0 <= held <= balance", id, a, b)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var entry int64
		err = tx.QueryRow(ctx, `SELECT id, account_id, balance_after, held_after FROM ledger_entries
			WHERE held_after < 0 OR balance_after < held_after ORDER BY id LIMIT 1`).Scan(&entry, &id, &a, &b)
		if err == nil {
			return fmt.Errorf("audit I1: entry %d (account %s) balance_after=%d held_after=%d violates 0 <= held <= balance", entry, id, a, b)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		// I2: balance = deposits - settled; held = holds - releases - held part
		// of settles = sum of active holds.
		err = tx.QueryRow(ctx, `
			WITH e AS (
				SELECT e.account_id,
					SUM(CASE e.kind WHEN 'deposit' THEN e.amount_nanos WHEN 'settle' THEN -e.amount_nanos ELSE 0 END)::bigint AS bal,
					SUM(CASE e.kind WHEN 'hold' THEN e.amount_nanos WHEN 'release' THEN -e.amount_nanos
						WHEN 'settle' THEN -LEAST(e.amount_nanos, h.amount_nanos) ELSE 0 END)::bigint AS held
				FROM ledger_entries e LEFT JOIN holds h ON h.id = e.hold_id GROUP BY e.account_id),
			hh AS (SELECT account_id, SUM(amount_nanos)::bigint AS held FROM holds WHERE status='active' GROUP BY account_id)
			SELECT a.id, a.balance_nanos, COALESCE(e.bal,0), a.held_nanos, COALESCE(e.held,0)
			FROM accounts a LEFT JOIN e ON e.account_id=a.id LEFT JOIN hh ON hh.account_id=a.id
			WHERE a.balance_nanos <> COALESCE(e.bal,0) OR a.held_nanos <> COALESCE(e.held,0) OR a.held_nanos <> COALESCE(hh.held,0)
			ORDER BY a.id LIMIT 1`).Scan(&id, &a, &b, &c, &d)
		if err == nil {
			return fmt.Errorf("audit I2: account %s balance=%d (entries say %d), held=%d (entries say %d, active holds disagree or match neither)", id, a, b, c, d)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		// The newest entry must describe the account as it is now.
		err = tx.QueryRow(ctx, `SELECT a.id, a.balance_nanos, l.balance_after, a.held_nanos, l.held_after
			FROM accounts a JOIN LATERAL (SELECT balance_after, held_after FROM ledger_entries
				WHERE account_id=a.id ORDER BY id DESC LIMIT 1) l ON true
			WHERE l.balance_after <> a.balance_nanos OR l.held_after <> a.held_nanos
			ORDER BY a.id LIMIT 1`).Scan(&id, &a, &b, &c, &d)
		if err == nil {
			return fmt.Errorf("audit I2: account %s balance=%d held=%d but last entry says balance=%d held=%d", id, a, c, b, d)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		// I3: each hold has one hold entry and at most one terminal outcome,
		// and the status agrees with its entries.
		var status string
		err = tx.QueryRow(ctx, `SELECT h.id, h.status FROM holds h LEFT JOIN LATERAL (
				SELECT count(*) FILTER (WHERE kind='hold') AS n_hold,
				       count(*) FILTER (WHERE kind='settle') AS n_settle,
				       count(*) FILTER (WHERE kind='release') AS n_release,
				       COALESCE(sum(amount_nanos) FILTER (WHERE kind='settle'),0) AS settle_amt
				FROM ledger_entries WHERE hold_id=h.id) x ON true
			WHERE x.n_hold <> 1
			   OR (h.status='active'   AND (x.n_settle > 0 OR x.n_release > 0))
			   OR (h.status='settled'  AND (x.n_settle <> 1 OR x.n_release > 1 OR x.settle_amt <> h.settled_nanos))
			   OR (h.status='released' AND (x.n_settle > 0 OR x.n_release <> 1))
			ORDER BY h.id LIMIT 1`).Scan(&id, &status)
		if err == nil {
			return fmt.Errorf("audit I3: hold %s (status %s) has entries that do not match a single terminal state", id, status)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return nil
	})
}
