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
	ServiceMessageID int64      `db:"service_message_id"`
	CorrectOption    int        `db:"correct_option"`
	Attempts         int        `db:"attempts"`
	State            string     `db:"state"` // pending | solved | kicked | cancelled
	JoinedAt         time.Time  `db:"joined_at"`
	Deadline         time.Time  `db:"deadline"`
	ProbationUntil   *time.Time `db:"probation_until"`
	ReleasedAt       *time.Time `db:"released_at"`
}

const gateCols = `id, chat_id, tg_user_id, username, captcha_message_id, service_message_id, correct_option,
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

// SetServiceMessage привязывает id service-сообщения «теперь в группе», чтобы удалить
// его при кике/уходе новичка (не оставлять след входа в чате).
func (r *Repository) SetServiceMessage(ctx context.Context, id, messageID int64) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE newcomer_gates SET service_message_id = $2 WHERE id = $1`, id, messageID); err != nil {
		return fmt.Errorf("set service message: %w", err)
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
	ID              int64      `db:"id"`
	ChatID          int64      `db:"chat_id"`
	MessageID       int64      `db:"message_id"`
	TGUserID        int64      `db:"tg_user_id"`
	Username        string     `db:"username"`
	Reason          string     `db:"reason"`
	Action          string     `db:"action"` // warn | restrict
	DetectedAt      time.Time  `db:"detected_at"`
	RestrictUntil   *time.Time `db:"restrict_until"`
	ReleasedAt      *time.Time `db:"released_at"`
	WarnMessageID   int64      `db:"warn_message_id"`
	SpamCount       int        `db:"spam_count"`
	OkCount         int        `db:"ok_count"`
	FalsePositive   bool       `db:"false_positive"`
	FalsePositiveAt *time.Time `db:"false_positive_at"`
	EscalatedAt     *time.Time `db:"escalated_at"`
	AdminAction     *string    `db:"admin_action"` // banned | restored | NULL
}

const spamCols = `id, chat_id, message_id, tg_user_id, username, reason, action,
	detected_at, restrict_until, released_at, warn_message_id, spam_count, ok_count,
	false_positive, false_positive_at, escalated_at, admin_action`

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

// CountSpamByUser — действующие предупреждения автора с since (для WarnMax).
// Снятые сообществом (false_positive) не наращивают prior.
func (r *Repository) CountSpamByUser(ctx context.Context, userID int64, since time.Time) (int, error) {
	var n int
	if err := r.db.GetContext(ctx, &n,
		`SELECT count(*) FROM spam_flags
		 WHERE tg_user_id = $1 AND detected_at >= $2 AND NOT false_positive`,
		userID, since); err != nil {
		return 0, fmt.Errorf("count spam: %w", err)
	}
	return n, nil
}

// SpamRestrictsDue — истёкшие рестрикты к снятию (без фильтра action: эскалация
// ставит restrict_until и на warn-строках; индекс idx_spam_restricts_due).
// Строка не выходит, если у того же автора есть более поздний действующий рестрикт:
// снятие раннего размьютило бы юзера в обход позднего.
func (r *Repository) SpamRestrictsDue(ctx context.Context, now time.Time) ([]SpamFlag, error) {
	var fs []SpamFlag
	if err := r.db.SelectContext(ctx, &fs,
		`SELECT `+spamCols+` FROM spam_flags s
		WHERE s.restrict_until IS NOT NULL
		  AND s.restrict_until < $1 AND s.released_at IS NULL
		  AND NOT EXISTS (
			SELECT 1 FROM spam_flags s2
			WHERE s2.tg_user_id = s.tg_user_id
			  AND s2.id <> s.id
			  AND s2.restrict_until IS NOT NULL
			  AND s2.released_at IS NULL
			  AND s2.restrict_until > s.restrict_until
			  AND s2.restrict_until > $1
		  )`, now); err != nil {
		return nil, fmt.Errorf("spam restricts due: %w", err)
	}
	return fs, nil
}

// SpamFlagsPendingDecision — флаги с достигнутым порогом голосов, но без решения
// (крэш между CastSpamBallot и claim оставил бы порог зависшим). Окно window
// ограничивает глубину, LIMIT — объём одной выборки свипера.
func (r *Repository) SpamFlagsPendingDecision(ctx context.Context, now time.Time, escSpam, escOk int, window time.Duration) ([]SpamFlag, error) {
	var fs []SpamFlag
	if err := r.db.SelectContext(ctx, &fs,
		`SELECT `+spamCols+` FROM spam_flags
		WHERE (spam_count >= $1 OR ok_count >= $2)
		  AND escalated_at IS NULL
		  AND false_positive = false
		  AND admin_action IS NULL
		  AND detected_at > $3
		ORDER BY id
		LIMIT 20`, escSpam, escOk, now.Add(-window)); err != nil {
		return nil, fmt.Errorf("spam flags pending decision: %w", err)
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

// GetSpamFlag возвращает флаг по id. Не найдено → (nil, nil).
func (r *Repository) GetSpamFlag(ctx context.Context, id int64) (*SpamFlag, error) {
	var f SpamFlag
	if err := r.db.GetContext(ctx, &f,
		`SELECT `+spamCols+` FROM spam_flags WHERE id = $1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get spam flag: %w", err)
	}
	return &f, nil
}

// SetSpamWarnMessage привязывает id поста-предупреждения (для правки при эскалации/снятии).
func (r *Repository) SetSpamWarnMessage(ctx context.Context, id, messageID int64) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE spam_flags SET warn_message_id = $2 WHERE id = $1`, id, messageID); err != nil {
		return fmt.Errorf("set spam warn message: %w", err)
	}
	return nil
}

// CastSpamBallot учитывает голос по флагу. Возвращает обновлённые счётчики и dup=true,
// если участник уже голосовал (PK spam_ballots). Атомарно в одной транзакции.
func (r *Repository) CastSpamBallot(ctx context.Context, flagID, userID int64, choice string) (spamN, okN int, dup bool, err error) {
	if choice != "spam" && choice != "ok" {
		return 0, 0, false, fmt.Errorf("invalid spam ballot choice: %q", choice)
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
		INSERT INTO spam_ballots (flag_id, user_id, choice) VALUES ($1, $2, $3)
		ON CONFLICT (flag_id, user_id) DO NOTHING
	`, flagID, userID, choice)
	if qerr != nil {
		err = fmt.Errorf("insert spam ballot: %w", qerr)
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		dup = true
		err = tx.Commit()
		return
	}

	col := "spam_count = spam_count + 1"
	if choice == "ok" {
		col = "ok_count = ok_count + 1"
	}
	type counts struct {
		SpamCount int `db:"spam_count"`
		OkCount   int `db:"ok_count"`
	}
	var c counts
	if qerr = tx.GetContext(ctx, &c,
		`UPDATE spam_flags SET `+col+` WHERE id = $1 RETURNING spam_count, ok_count`, flagID); qerr != nil {
		err = fmt.Errorf("update spam counts: %w", qerr)
		return
	}
	spamN = c.SpamCount
	okN = c.OkCount
	err = tx.Commit()
	return
}

// ClaimSpamEscalation атомарно захватывает переход в escalated (один раз, даже при
// гонке решающих голосов). Возвращает claimed=true, только если строка была обновлена.
func (r *Repository) ClaimSpamEscalation(ctx context.Context, id int64) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE spam_flags SET escalated_at = now()
		 WHERE id = $1 AND escalated_at IS NULL AND false_positive = false`, id)
	if err != nil {
		return false, fmt.Errorf("claim spam escalation: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClaimSpamFalsePositive атомарно помечает ложную тревогу (взаимоисключающе с эскалацией).
func (r *Repository) ClaimSpamFalsePositive(ctx context.Context, id int64) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE spam_flags SET false_positive = true, false_positive_at = now()
		 WHERE id = $1 AND escalated_at IS NULL AND false_positive = false`, id)
	if err != nil {
		return false, fmt.Errorf("claim spam false positive: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClaimSpamAdminAction атомарно фиксирует решение админа после эскалации.
// 'restored' разрешён и поверх 'banned' (админ ошибся с баном — кнопка исправляет);
// 'banned' — только по чистому NULL.
func (r *Repository) ClaimSpamAdminAction(ctx context.Context, id int64, action string) (bool, error) {
	cond := "admin_action IS NULL"
	if action == "restored" {
		cond = "(admin_action IS NULL OR admin_action = 'banned')"
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE spam_flags SET admin_action = $2
		 WHERE id = $1 AND escalated_at IS NOT NULL AND (`+cond+`)`, id, action)
	if err != nil {
		return false, fmt.Errorf("claim spam admin action: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ResetSpamAdminAction откатывает claim решения админа (TG-вызов упал — путь должен
// остаться открытым для повторного клика). Стирает только наше же действие, чтобы
// не затереть параллельное решение другого админа.
func (r *Repository) ResetSpamAdminAction(ctx context.Context, id int64, action string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE spam_flags SET admin_action = NULL WHERE id = $1 AND admin_action = $2`, id, action)
	if err != nil {
		return fmt.Errorf("reset spam admin action: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("reset spam admin action: claim %d not found or replaced", id)
	}
	return nil
}

// SetSpamRestrict ставит/продлевает restrict_until на строке флага (эскалация).
// released_at сбрасывается: истёкший и отрелизенный рестрикт, продлённый эскалацией,
// должен снова попасть в выборку свипера SpamRestrictsDue.
func (r *Repository) SetSpamRestrict(ctx context.Context, id int64, until time.Time) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE spam_flags SET restrict_until = $2, released_at = NULL WHERE id = $1`, id, until); err != nil {
		return fmt.Errorf("set spam restrict: %w", err)
	}
	return nil
}

// SpamVoter — проголосовавший «спам» по флагу (для уведомления админов).
type SpamVoter struct {
	UserID   int64  `db:"user_id"`
	Username string `db:"username"`
}

// SpamVoters — голоса «спам» по флагу, в порядке подачи.
func (r *Repository) SpamVoters(ctx context.Context, flagID int64) ([]SpamVoter, error) {
	var vs []SpamVoter
	if err := r.db.SelectContext(ctx, &vs, `
		SELECT b.user_id, COALESCE(u.username, '') AS username
		FROM spam_ballots b
		LEFT JOIN users u ON u.tg_user_id = b.user_id
		WHERE b.flag_id = $1 AND b.choice = 'spam'
		ORDER BY b.voted_at
	`, flagID); err != nil {
		return nil, fmt.Errorf("spam voters: %w", err)
	}
	return vs, nil
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
