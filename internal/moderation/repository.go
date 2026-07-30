package moderation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// VerdictRecord — строка таблицы newcomer_verdicts.
type VerdictRecord struct {
	ID          int64      `db:"id"`
	TGUserID    int64      `db:"tg_user_id"`
	ChatID      int64      `db:"chat_id"`
	Question    string     `db:"question"`
	Answer      string     `db:"answer"`
	Verdict     string     `db:"verdict"`
	Reason      string     `db:"reason"`
	AdminAction *string    `db:"admin_action"` // kicked | kept | NULL
	CreatedAt   time.Time  `db:"created_at"`
	DecidedAt   *time.Time `db:"decided_at"`
}

// Repository хранит вердикты модерации.
type Repository struct {
	db *sqlx.DB
}

// NewRepository создаёт repository.
func NewRepository(db *sqlx.DB) *Repository { return &Repository{db: db} }

// SaveVerdict записывает вердикт. Возвращает id для привязки inline-keyboard callback.
func (r *Repository) SaveVerdict(ctx context.Context, rec *VerdictRecord) (int64, error) {
	var id int64
	err := r.db.GetContext(ctx, &id, `
		INSERT INTO newcomer_verdicts (tg_user_id, chat_id, question, answer, verdict, reason)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
	`, rec.TGUserID, rec.ChatID, rec.Question, rec.Answer, rec.Verdict, rec.Reason)
	if err != nil {
		return 0, fmt.Errorf("save verdict: %w", err)
	}
	return id, nil
}

// SetAdminAction отмечает решение админа (kicked/kept) и проставляет decided_at.
func (r *Repository) SetAdminAction(ctx context.Context, id int64, action string) error {
	if _, err := r.db.ExecContext(ctx, `
		UPDATE newcomer_verdicts
		SET admin_action = $2, decided_at = now()
		WHERE id = $1
	`, id, action); err != nil {
		return fmt.Errorf("set admin action: %w", err)
	}
	return nil
}

// Get возвращает вердикт по id.
func (r *Repository) Get(ctx context.Context, id int64) (*VerdictRecord, error) {
	var v VerdictRecord
	if err := r.db.GetContext(ctx, &v, `
		SELECT id, tg_user_id, chat_id, question, answer, verdict, reason,
		       admin_action, created_at, decided_at
		FROM newcomer_verdicts WHERE id = $1
	`, id); err != nil {
		return nil, fmt.Errorf("get verdict: %w", err)
	}
	return &v, nil
}

// --- гейт новичков (newcomer_gates) ---

// GateRecord — строка newcomer_gates.
type GateRecord struct {
	ID               int64      `db:"id"`
	ChatID           int64      `db:"chat_id"`
	TGUserID         int64      `db:"tg_user_id"`
	Username         string     `db:"username"`
	CaptchaMessageID int64      `db:"captcha_message_id"`
	CorrectOption    int        `db:"correct_option"`
	Attempts         int        `db:"attempts"`
	State            string     `db:"state"` // pending | solved | kicked | cancelled
	JoinedAt         time.Time  `db:"joined_at"`
	Deadline         time.Time  `db:"deadline"`
	ProbationUntil   *time.Time `db:"probation_until"`
	ReleasedAt       *time.Time `db:"released_at"`
}

const gateCols = `id, chat_id, tg_user_id, username, captcha_message_id, correct_option,
	attempts, state, joined_at, deadline, probation_until, released_at`

// CreateGate создаёт запись гейта, возвращает id для привязки кнопок.
func (r *Repository) CreateGate(ctx context.Context, chatID, userID int64, username string, correct int, deadline time.Time) (int64, error) {
	var id int64
	err := r.db.GetContext(ctx, &id, `
		INSERT INTO newcomer_gates (chat_id, tg_user_id, username, correct_option, deadline)
		VALUES ($1, $2, $3, $4, $5) RETURNING id
	`, chatID, userID, username, correct, deadline)
	if err != nil {
		return 0, fmt.Errorf("create gate: %w", err)
	}
	return id, nil
}

// SetCaptchaMessage привязывает id отправленного сообщения с капчей.
func (r *Repository) SetCaptchaMessage(ctx context.Context, id, messageID int64) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE newcomer_gates SET captcha_message_id = $2 WHERE id = $1`, id, messageID); err != nil {
		return fmt.Errorf("set captcha message: %w", err)
	}
	return nil
}

// GetGate возвращает запись гейта по id.
func (r *Repository) GetGate(ctx context.Context, id int64) (*GateRecord, error) {
	var g GateRecord
	if err := r.db.GetContext(ctx, &g,
		`SELECT `+gateCols+` FROM newcomer_gates WHERE id = $1`, id); err != nil {
		return nil, fmt.Errorf("get gate: %w", err)
	}
	return &g, nil
}

// IncAttempts инкрементирует счётчик неверных попыток, возвращает новое значение.
func (r *Repository) IncAttempts(ctx context.Context, id int64) (int, error) {
	var n int
	if err := r.db.GetContext(ctx, &n,
		`UPDATE newcomer_gates SET attempts = attempts + 1 WHERE id = $1 RETURNING attempts`, id); err != nil {
		return 0, fmt.Errorf("inc attempts: %w", err)
	}
	return n, nil
}

// SetGateSolved помечает капчу решённой и ставит срок линк-провации.
func (r *Repository) SetGateSolved(ctx context.Context, id int64, probationUntil time.Time) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE newcomer_gates SET state = 'solved', probation_until = $2 WHERE id = $1`, id, probationUntil); err != nil {
		return fmt.Errorf("set gate solved: %w", err)
	}
	return nil
}

// SetGateKicked / SetGateCancelled — терминальные состояния.
func (r *Repository) SetGateKicked(ctx context.Context, id int64) error {
	return r.setGateState(ctx, id, "kicked")
}
func (r *Repository) SetGateCancelled(ctx context.Context, id int64) error {
	return r.setGateState(ctx, id, "cancelled")
}
func (r *Repository) setGateState(ctx context.Context, id int64, state string) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE newcomer_gates SET state = $2 WHERE id = $1`, id, state); err != nil {
		return fmt.Errorf("set gate state: %w", err)
	}
	return nil
}

// SetGateReleased отмечает снятие линк-провации (released_at), состояние остаётся solved.
func (r *Repository) SetGateReleased(ctx context.Context, id int64) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE newcomer_gates SET released_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("set gate released: %w", err)
	}
	return nil
}

// PendingDue — просроченные pending (дедлайн прошёл) → кик.
func (r *Repository) PendingDue(ctx context.Context, now time.Time) ([]GateRecord, error) {
	var gs []GateRecord
	if err := r.db.SelectContext(ctx, &gs,
		`SELECT `+gateCols+` FROM newcomer_gates WHERE state = 'pending' AND deadline < $1`, now); err != nil {
		return nil, fmt.Errorf("pending due: %w", err)
	}
	return gs, nil
}

// PendingNotDue — pending, чей дедлайн ещё не прошёл (для cleanup-проверки через getChatMember).
func (r *Repository) PendingNotDue(ctx context.Context, now time.Time) ([]GateRecord, error) {
	var gs []GateRecord
	if err := r.db.SelectContext(ctx, &gs,
		`SELECT `+gateCols+` FROM newcomer_gates WHERE state = 'pending' AND deadline >= $1`, now); err != nil {
		return nil, fmt.Errorf("pending not due: %w", err)
	}
	return gs, nil
}

// ProbationDue — решённые с истёкшей линк-провацией, ограничение ещё не снято.
func (r *Repository) ProbationDue(ctx context.Context, now time.Time) ([]GateRecord, error) {
	var gs []GateRecord
	if err := r.db.SelectContext(ctx, &gs,
		`SELECT `+gateCols+` FROM newcomer_gates
		WHERE state = 'solved' AND probation_until IS NOT NULL AND probation_until < $1 AND released_at IS NULL`, now); err != nil {
		return nil, fmt.Errorf("probation due: %w", err)
	}
	return gs, nil
}

// PendingGateForUser — активная капча пользователя в чате (для cleanup при уходе).
// Не найдено → (nil, nil).
func (r *Repository) PendingGateForUser(ctx context.Context, chatID, userID int64) (*GateRecord, error) {
	var g GateRecord
	if err := r.db.GetContext(ctx, &g,
		`SELECT `+gateCols+` FROM newcomer_gates
		WHERE chat_id = $1 AND tg_user_id = $2 AND state = 'pending'
		ORDER BY id DESC LIMIT 1`, chatID, userID); err != nil {
		return nil, nil
	}
	return &g, nil
}

// --- анти-спам (spam_flags) ---

// SpamFlag — строка таблицы spam_flags.
type SpamFlag struct {
	ID            int64      `db:"id"`
	ChatID        int64      `db:"chat_id"`
	MessageID     int64      `db:"message_id"`
	TGUserID      int64      `db:"tg_user_id"`
	Username      string     `db:"username"`
	Reason        string     `db:"reason"`
	Action        string     `db:"action"` // warn | restrict
	DetectedAt    time.Time  `db:"detected_at"`
	RestrictUntil *time.Time `db:"restrict_until"`
	ReleasedAt    *time.Time `db:"released_at"`
}

const spamCols = `id, chat_id, message_id, tg_user_id, username, reason, action,
	detected_at, restrict_until, released_at`

func (r *Repository) CreateSpamFlag(ctx context.Context, chatID, messageID, userID int64, username, reason, action string, restrictUntil *time.Time) (int64, error) {
	var id int64
	err := r.db.GetContext(ctx, &id, `
		INSERT INTO spam_flags (chat_id, message_id, tg_user_id, username, reason, action, restrict_until)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id
	`, chatID, messageID, userID, username, reason, action, restrictUntil)
	if err != nil {
		return 0, fmt.Errorf("create spam flag: %w", err)
	}
	return id, nil
}

func (r *Repository) CountSpamByUser(ctx context.Context, userID int64, since time.Time) (int, error) {
	var n int
	if err := r.db.GetContext(ctx, &n,
		`SELECT count(*) FROM spam_flags WHERE tg_user_id = $1 AND detected_at >= $2`,
		userID, since); err != nil {
		return 0, fmt.Errorf("count spam: %w", err)
	}
	return n, nil
}

func (r *Repository) SpamRestrictsDue(ctx context.Context, now time.Time) ([]SpamFlag, error) {
	var fs []SpamFlag
	if err := r.db.SelectContext(ctx, &fs,
		`SELECT `+spamCols+` FROM spam_flags
		WHERE action = 'restrict' AND restrict_until IS NOT NULL
		  AND restrict_until < $1 AND released_at IS NULL`, now); err != nil {
		return nil, fmt.Errorf("spam restricts due: %w", err)
	}
	return fs, nil
}

func (r *Repository) ReleaseSpamRestrict(ctx context.Context, id int64) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE spam_flags SET released_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("release spam restrict: %w", err)
	}
	return nil
}

// --- голосование за изгнание (kick_votes / vote_ballots) ---

// VoteRecord — строка таблицы kick_votes.
type VoteRecord struct {
	ID             int64      `db:"id"`
	ChatID         int64      `db:"chat_id"`
	TargetUserID   int64      `db:"target_user_id"`
	TargetUsername string     `db:"target_username"`
	Reason         string     `db:"reason"`
	MessageID      int64      `db:"message_id"`
	ForCount       int        `db:"for_count"`
	AgainstCount   int        `db:"against_count"`
	CreatedAt      time.Time  `db:"created_at"`
	ClosesAt       time.Time  `db:"closes_at"`
	ResolvedAt     *time.Time `db:"resolved_at"`
	Outcome        string     `db:"outcome"` // open | kicked | closed
}

const voteCols = `id, chat_id, target_user_id, target_username, reason, message_id,
	for_count, against_count, created_at, closes_at, resolved_at, outcome`

func (r *Repository) CreateVote(ctx context.Context, chatID, targetUserID int64, targetUsername, reason string, closesAt time.Time) (int64, error) {
	var id int64
	err := r.db.GetContext(ctx, &id, `
		INSERT INTO kick_votes (chat_id, target_user_id, target_username, reason, closes_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`, chatID, targetUserID, targetUsername, reason, closesAt)
	if err != nil {
		return 0, fmt.Errorf("create vote: %w", err)
	}
	return id, nil
}

func (r *Repository) GetVote(ctx context.Context, id int64) (*VoteRecord, error) {
	var v VoteRecord
	if err := r.db.GetContext(ctx, &v,
		`SELECT `+voteCols+` FROM kick_votes WHERE id = $1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get vote: %w", err)
	}
	return &v, nil
}

func (r *Repository) SetVoteMessage(ctx context.Context, id, messageID int64) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE kick_votes SET message_id = $2 WHERE id = $1`, id, messageID); err != nil {
		return fmt.Errorf("set vote message: %w", err)
	}
	return nil
}

// CastBallot учитывает голос. Возвращает обновлённые счётчики и dup=true, если
// пользователь уже голосовал (PK vote_ballots). Атомарно в одной транзакции.
func (r *Repository) CastBallot(ctx context.Context, voteID, userID int64, choice string) (forN, againstN int, dup bool, err error) {
	if choice != "for" && choice != "against" {
		return 0, 0, false, fmt.Errorf("invalid ballot choice: %q", choice)
	}
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, 0, false, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	res, qerr := tx.ExecContext(ctx, `
		INSERT INTO vote_ballots (vote_id, user_id, choice) VALUES ($1, $2, $3)
		ON CONFLICT (vote_id, user_id) DO NOTHING
	`, voteID, userID, choice)
	if qerr != nil {
		err = fmt.Errorf("insert ballot: %w", qerr)
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		dup = true
		err = tx.Commit()
		return
	}

	col := "for_count = for_count + 1"
	if choice == "against" {
		col = "against_count = against_count + 1"
	}
	type counts struct {
		ForCount     int `db:"for_count"`
		AgainstCount int `db:"against_count"`
	}
	var c counts
	if qerr = tx.GetContext(ctx, &c,
		`UPDATE kick_votes SET `+col+` WHERE id = $1 RETURNING for_count, against_count`, voteID); qerr != nil {
		err = fmt.Errorf("update vote counts: %w", qerr)
		return
	}
	forN = c.ForCount
	againstN = c.AgainstCount
	err = tx.Commit()
	return
}

func (r *Repository) VotesDue(ctx context.Context, now time.Time) ([]VoteRecord, error) {
	var vs []VoteRecord
	if err := r.db.SelectContext(ctx, &vs,
		`SELECT `+voteCols+` FROM kick_votes WHERE outcome = 'open' AND closes_at < $1`, now); err != nil {
		return nil, fmt.Errorf("votes due: %w", err)
	}
	return vs, nil
}

// ResolveVote атомарно переводит голосование из 'open' в итоговое состояние.
// Возвращает claimed=true, только если строка была обновлена (т.е. переход захвачен
// этим вызовом). Гарантирует, что кик/итог сработает ровно один раз при гонке.
func (r *Repository) ResolveVote(ctx context.Context, id int64, outcome string) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE kick_votes SET outcome = $2, resolved_at = now()
		 WHERE id = $1 AND outcome = 'open'`, id, outcome)
	if err != nil {
		return false, fmt.Errorf("resolve vote: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ActiveVoteForTarget — активное (open) голосование за пользователя в чате.
// Не найдено → (nil, nil).
func (r *Repository) ActiveVoteForTarget(ctx context.Context, chatID, targetUserID int64) (*VoteRecord, error) {
	var v VoteRecord
	if err := r.db.GetContext(ctx, &v,
		`SELECT `+voteCols+` FROM kick_votes
		WHERE chat_id = $1 AND target_user_id = $2 AND outcome = 'open'
		ORDER BY id DESC LIMIT 1`, chatID, targetUserID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("active vote for target: %w", err)
	}
	return &v, nil
}
