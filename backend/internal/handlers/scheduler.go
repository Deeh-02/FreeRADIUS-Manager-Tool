package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/freeradius-manager/backend/internal/database"
	"github.com/freeradius-manager/backend/internal/settings"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// ScheduledTask represents an automated background job.
type ScheduledTask struct {
	ID          int        `json:"id"`
	Name        string     `json:"name"`
	TaskType    string     `json:"task_type"`
	Description string     `json:"description"`
	Schedule    string     `json:"schedule"`
	IsActive    bool       `json:"is_active"`
	LastRun     *time.Time `json:"last_run"`
	NextRun     *time.Time `json:"next_run"`
	LastResult  *string    `json:"last_result"`
	CreatedAt   time.Time  `json:"created_at"`
}

// taskDescriptions is shown on the Scheduler page.
var taskDescriptions = map[string]string{
	"expire_accounts":       "Marks customers past their paid-until date (+ grace days) as expired, blocks them at the RADIUS server and disconnects their sessions.",
	"mark_overdue_invoices": "Moves unpaid invoices past their due date to overdue. With the suspend_overdue setting, also blocks customers whose service was started on credit.",
	"cleanup_sessions":      "Closes accounting sessions that have had no update for 24 hours (the router never sent a Stop), so they stop counting against the device limit.",
	"daily_report":          "Counts active customers, open sessions, today's traffic and today's payments; emails it to the report_email setting if set.",
	"renew_plans":           "Raises the next invoice for customers on a paid plan whose paid-until date is within renewal_invoice_days. Free plans are extended automatically.",
	"expiry_reminders":      "Sends one SMS (phone + SMS gateway) and/or email (address + SMTP) per period, reminder_days before the paid-until date.",
	"voucher_housekeeping":  "Marks vouchers used after their first session and expired after their expiry date.",
	"enforce_quota":         "Sends a Disconnect-Request for online sessions of customers who have used up their data cap (the cap is otherwise checked only at login).",
	"backup_database":       "Writes a pg_dump to BACKUP_DIR and deletes dumps older than backup_retention_days.",
	"purge_logs":            "Deletes login logs (radpostauth) older than 180 days, honeypot logs older than 30 days, and expired portal sessions.",
}

var validSchedules = map[string]bool{"hourly": true, "daily": true, "weekly": true}

// ListScheduledTasks returns all configured tasks.
func (h *Handler) ListScheduledTasks(c *gin.Context) {
	rows, err := h.db.Query(`
		SELECT id, name, task_type, schedule, is_active, last_run, next_run, last_result, created_at
		FROM scheduled_tasks ORDER BY id`)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to fetch tasks")
		return
	}
	defer rows.Close()

	tasks := []ScheduledTask{}
	for rows.Next() {
		var t ScheduledTask
		if rows.Scan(&t.ID, &t.Name, &t.TaskType, &t.Schedule, &t.IsActive, &t.LastRun, &t.NextRun, &t.LastResult, &t.CreatedAt) == nil {
			t.Description = taskDescriptions[t.TaskType]
			tasks = append(tasks, t)
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": tasks})
}

// ToggleTask enables or disables a scheduled task.
func (h *Handler) ToggleTask(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var active bool
	if err := h.db.QueryRow(`UPDATE scheduled_tasks SET is_active = NOT is_active WHERE id=$1 RETURNING is_active`, id).Scan(&active); err != nil {
		respondError(c, http.StatusNotFound, "task not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "task toggled", "is_active": active})
}

// RunTaskNow forces immediate execution of a task.
func (h *Handler) RunTaskNow(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var taskType string
	if err := h.db.QueryRow(`SELECT task_type FROM scheduled_tasks WHERE id=$1`, id).Scan(&taskType); err != nil {
		respondError(c, http.StatusNotFound, "task not found")
		return
	}

	result, count, ran := runTaskLocked(h.db, h.log, id, taskType)
	if !ran {
		respondError(c, http.StatusConflict, "this task is already running")
		return
	}
	h.db.Exec(`UPDATE scheduled_tasks SET last_run=NOW(), last_result=$1 WHERE id=$2`,
		fmt.Sprintf("%s (%d)", result, count), id)

	c.JSON(http.StatusOK, gin.H{
		"message":  result,
		"affected": count,
	})
}

// UpdateTaskSchedule changes the schedule of a task (hourly, daily or weekly).
func (h *Handler) UpdateTaskSchedule(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Schedule string `json:"schedule" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if !validSchedules[req.Schedule] {
		respondError(c, http.StatusBadRequest, "schedule must be hourly, daily or weekly")
		return
	}
	nextRun := nextRunTime(req.Schedule)
	res, err := h.db.Exec(`UPDATE scheduled_tasks SET schedule=$1, next_run=$2 WHERE id=$3`, req.Schedule, nextRun, id)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to update schedule")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(c, http.StatusNotFound, "task not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "schedule updated", "next_run": nextRun})
}

// StartScheduler launches the background task runner.
// It is an in-process poller, not cron: every 10 minutes it runs the active tasks whose
// next_run is empty or past. "daily" therefore means "24h after the last run", not a fixed
// time of day. Each task takes a PostgreSQL advisory lock while it runs, so if several
// backend copies run the scheduler, a task still runs only once at a time.
func StartScheduler(db *database.DB, log *logrus.Logger) {
	go func() {
		log.Info("Scheduler started — checking tasks every 10 minutes")
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()

		runDueTasks(db, log)
		for range ticker.C {
			runDueTasks(db, log)
		}
	}()
}

type dueTask struct {
	id       int
	taskType string
	schedule string
}

// listDueTasks returns the active tasks whose next_run is empty or past.
func listDueTasks(db *database.DB) ([]dueTask, error) {
	rows, err := db.Query(`
		SELECT id, task_type, schedule FROM scheduled_tasks
		WHERE is_active=true AND (next_run IS NULL OR next_run <= NOW())
		ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []dueTask
	for rows.Next() {
		var t dueTask
		if rows.Scan(&t.id, &t.taskType, &t.schedule) == nil {
			tasks = append(tasks, t)
		}
	}
	return tasks, nil
}

// claimLease is how long a claimed task is reserved for the backend that claimed it. If that
// backend dies mid-task, the task becomes due again after the lease.
const claimLease = 15 * time.Minute

// claimTask atomically reserves a due task for this backend. Two backend copies that read the
// same due list both try; only one UPDATE matches, because the first one moves next_run into
// the future. Without this a task could run twice, back to back, once the first copy had
// released the advisory lock.
func claimTask(db *database.DB, id int) bool {
	res, err := db.Exec(`UPDATE scheduled_tasks SET next_run = NOW() + $2::interval
		WHERE id = $1 AND is_active AND (next_run IS NULL OR next_run <= NOW())`,
		id, fmt.Sprintf("%d seconds", int(claimLease.Seconds())))
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

func runDueTasks(db *database.DB, log *logrus.Logger) {
	tasks, err := listDueTasks(db)
	if err != nil {
		log.WithError(err).Warn("scheduler: cannot read tasks")
		return
	}
	runTaskList(db, log, tasks)
}

func runTaskList(db *database.DB, log *logrus.Logger, tasks []dueTask) {
	for _, t := range tasks {
		if !claimTask(db, t.id) {
			continue // another backend took it
		}
		result, count, ran := runTaskLocked(db, log, t.id, t.taskType)
		if !ran {
			// Still running elsewhere (e.g. started by hand): give the claim back.
			db.Exec(`UPDATE scheduled_tasks SET next_run = NOW() WHERE id = $1`, t.id)
			continue
		}
		db.Exec(`UPDATE scheduled_tasks SET last_run=NOW(), next_run=$1, last_result=$2 WHERE id=$3`,
			nextRunTime(t.schedule), fmt.Sprintf("%s (%d)", result, count), t.id)
		log.Infof("Scheduler: ran '%s' — %s (%d)", t.taskType, result, count)
	}
}

// taskRunHook lets a test count how many times a task really executes. Nil in production.
var taskRunHook func(taskType string)

var localTaskLocks sync.Map

// runTaskLocked runs a task unless it is already running (in this process or, through the
// advisory lock, in another backend). ran is false if it was skipped.
func runTaskLocked(db *database.DB, log *logrus.Logger, id int, taskType string) (string, int, bool) {
	mu, _ := localTaskLocks.LoadOrStore(id, &sync.Mutex{})
	if !mu.(*sync.Mutex).TryLock() {
		return "", 0, false
	}
	defer mu.(*sync.Mutex).Unlock()

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return "failed: " + err.Error(), 0, true
	}
	defer conn.Close()
	var got bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(727501, $1)`, id).Scan(&got); err != nil || !got {
		return "", 0, false
	}
	defer conn.ExecContext(ctx, `SELECT pg_advisory_unlock(727501, $1)`, id)

	result, count := runTask(db, log, taskType)
	return result, count, true
}

// runTask executes the logic for a given task type and returns a description and a count.
func runTask(db *database.DB, log *logrus.Logger, taskType string) (string, int) {
	if taskRunHook != nil {
		taskRunHook(taskType)
	}
	switch taskType {

	case "expire_accounts":
		rows, err := db.Query(`
			UPDATE radius_users SET status='expired', updated_at=NOW()
			WHERE status='active'
			  AND account_expiry IS NOT NULL
			  AND account_expiry + $1::int < CURRENT_DATE
			RETURNING id, username`, graceDays())
		if err != nil {
			return "expire_accounts failed: " + err.Error(), 0
		}
		users := collectUsers(rows)
		for _, u := range users {
			if err := syncAccess(db, u.name); err != nil {
				log.WithError(err).Warnf("expire: sync %s", u.name)
			}
			disconnectUserSessions(db, u.name, false)
			go dispatchWebhookDB(db, "user.expired", gin.H{"id": u.id, "username": u.name})
		}
		return "accounts expired and blocked", len(users)

	case "mark_overdue_invoices":
		res, err := db.Exec(`
			UPDATE invoices SET status='overdue', updated_at=NOW()
			WHERE status='pending'
			  AND due_date IS NOT NULL
			  AND due_date < CURRENT_DATE`)
		if err != nil {
			return "mark_overdue failed: " + err.Error(), 0
		}
		n, _ := res.RowsAffected()
		if !settings.Bool("suspend_overdue", "", false) {
			return "invoices marked overdue", int(n)
		}
		// Postpaid customers (service given before payment) are blocked while overdue.
		rows, err := db.Query(`
			UPDATE radius_users ru SET status='expired', updated_at=NOW()
			WHERE ru.status='active' AND EXISTS (
			    SELECT 1 FROM invoices i
			    WHERE i.user_id = ru.id AND i.status='overdue' AND i.service_applied
			      AND i.due_date + $1::int < CURRENT_DATE)
			RETURNING ru.id, ru.username`, graceDays())
		if err != nil {
			return "suspend overdue failed: " + err.Error(), int(n)
		}
		users := collectUsers(rows)
		for _, u := range users {
			syncAccess(db, u.name)
			disconnectUserSessions(db, u.name, false)
		}
		return fmt.Sprintf("invoices marked overdue; %d customer(s) blocked for non-payment", len(users)), int(n)

	case "cleanup_sessions":
		res, err := db.Exec(`
			UPDATE radacct SET acctstoptime=NOW(),
			       acctterminatecause='Stale-Session'
			WHERE acctstoptime IS NULL
			  AND COALESCE(acctupdatetime, acctstarttime) < NOW() - INTERVAL '24 hours'`)
		if err != nil {
			return "cleanup_sessions failed: " + err.Error(), 0
		}
		n, _ := res.RowsAffected()
		return "stale sessions closed", int(n)

	case "daily_report":
		var users, sessions int
		var traffic int64
		db.QueryRow(`SELECT COUNT(*) FROM radius_users WHERE status='active'`).Scan(&users)
		db.QueryRow(`SELECT COUNT(*) FROM radacct WHERE acctstoptime IS NULL`).Scan(&sessions)
		db.QueryRow(`SELECT COALESCE(SUM(acctinputoctets+acctoutputoctets),0) FROM radacct WHERE acctstarttime >= CURRENT_DATE`).Scan(&traffic)
		var pay []string
		if rows, err := db.Query(`SELECT currency, SUM(amount), COUNT(*) FROM payments
			WHERE status='confirmed' AND created_at >= CURRENT_DATE - 1 AND created_at < CURRENT_DATE
			GROUP BY currency ORDER BY currency`); err == nil {
			for rows.Next() {
				var cur string
				var sum float64
				var cnt int
				if rows.Scan(&cur, &sum, &cnt) == nil {
					pay = append(pay, fmt.Sprintf("%s %.2f (%d)", cur, sum, cnt))
				}
			}
			rows.Close()
		}
		if len(pay) == 0 {
			pay = []string{"none"}
		}
		report := fmt.Sprintf("%d active customers, %d open sessions, %.1f MB today; payments yesterday: %s",
			users, sessions, float64(traffic)/1048576, strings.Join(pay, ", "))
		if to := settings.String("report_email", "", ""); to != "" && os.Getenv("SMTP_HOST") != "" {
			if err := sendEmail(to, "[RADIUS Manager] Daily report "+time.Now().Format("2006-01-02"), report); err != nil {
				report += " (email failed: " + err.Error() + ")"
			} else {
				report += " (emailed)"
			}
		}
		return report, 1

	case "renew_plans":
		return raiseRenewalInvoices(db, log)

	case "expiry_reminders":
		return sendExpiryReminders(db, log)

	case "voucher_housekeeping":
		used, _ := db.Exec(`
			UPDATE vouchers v SET status='used', redeemed_by=v.code,
			       redeemed_at=(SELECT MIN(acctstarttime) FROM radacct ra WHERE ra.username=v.code),
			       updated_at=NOW()
			WHERE v.status='active' AND EXISTS (SELECT 1 FROM radacct ra WHERE ra.username=v.code)`)
		expired, _ := db.Exec(`
			UPDATE vouchers SET status='expired', updated_at=NOW()
			WHERE status IN ('active','used') AND expires_at IS NOT NULL AND expires_at < NOW()`)
		var nu, ne int64
		if used != nil {
			nu, _ = used.RowsAffected()
		}
		if expired != nil {
			ne, _ = expired.RowsAffected()
		}
		return fmt.Sprintf("%d voucher(s) marked used, %d expired", nu, ne), int(nu + ne)

	case "enforce_quota":
		rows, err := db.Query(`
			SELECT DISTINCT rc.username FROM radcheck rc
			LEFT JOIN radius_users ru ON ru.username = rc.username
			WHERE rc.attribute = 'Max-Octets' AND rc.value ~ '^[0-9]+$'
			  AND EXISTS (SELECT 1 FROM radacct o WHERE o.username = rc.username AND o.acctstoptime IS NULL)
			  AND (SELECT COALESCE(SUM(acctinputoctets + acctoutputoctets), 0) FROM radacct ra
			       WHERE ra.username = rc.username
			         AND ra.acctstarttime >= COALESCE(ru.period_start, DATE '1970-01-01')) >= rc.value::numeric`)
		if err != nil {
			return "enforce_quota failed: " + err.Error(), 0
		}
		var names []string
		for rows.Next() {
			var u string
			if rows.Scan(&u) == nil {
				names = append(names, u)
			}
		}
		rows.Close()
		closed := 0
		for _, u := range names {
			n, _ := disconnectUserSessions(db, u, false)
			closed += n
		}
		return fmt.Sprintf("%d over-quota user(s), %d session(s) disconnected", len(names), closed), closed

	case "backup_database":
		name, err := runBackup()
		if err != nil {
			return "backup failed: " + err.Error(), 0
		}
		removed := pruneBackups(settings.Int("backup_retention_days", "BACKUP_RETENTION_DAYS", 14))
		return fmt.Sprintf("wrote %s, removed %d old backup(s)", name, removed), 1

	case "purge_logs":
		total := 0
		for _, q := range []string{
			`DELETE FROM radpostauth WHERE authdate < NOW() - INTERVAL '180 days'`,
			`DELETE FROM honeypot_logs WHERE created_at < NOW() - INTERVAL '30 days'`,
			`DELETE FROM portal_sessions WHERE expires_at < NOW()`,
			`DELETE FROM geoip_cache WHERE looked_up_at < NOW() - INTERVAL '2 days'`,
			`DELETE FROM webhook_logs WHERE created_at < NOW() - INTERVAL '90 days'`,
			`DELETE FROM refresh_tokens WHERE expires_at < NOW() - INTERVAL '7 days'`,
		} {
			if res, err := db.Exec(q); err == nil {
				n, _ := res.RowsAffected()
				total += int(n)
			}
		}
		return "old log rows deleted", total
	}

	return "unknown task", 0
}

type userRef struct {
	id   int
	name string
}

func collectUsers(rows *sql.Rows) []userRef {
	defer rows.Close()
	var out []userRef
	for rows.Next() {
		var u userRef
		if rows.Scan(&u.id, &u.name) == nil {
			out = append(out, u)
		}
	}
	return out
}

// raiseRenewalInvoices prepares the next period for customers on a plan. Paid plans get an
// invoice due on the paid-until date (the period is added when it is paid); free plans are
// extended directly. Nothing is ever extended for free on a paid plan.
func raiseRenewalInvoices(db *database.DB, log *logrus.Logger) (string, int) {
	days := settings.Int("renewal_invoice_days", "", 3)
	rows, err := db.Query(`
		SELECT ru.id, ru.username
		FROM radius_users ru
		JOIN user_plans up ON up.id = ru.plan_id AND up.is_active
		WHERE ru.status = 'active'
		  AND ru.account_expiry IS NOT NULL
		  AND ru.account_expiry BETWEEN CURRENT_DATE - 1 AND CURRENT_DATE + $1::int
		  AND NOT EXISTS (SELECT 1 FROM invoices i
		                  WHERE i.user_id = ru.id AND i.status IN ('pending','overdue'))`, days)
	if err != nil {
		return "renew_plans failed: " + err.Error(), 0
	}
	type cand struct {
		id   int
		name string
	}
	var list []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.id, &c.name) == nil {
			list = append(list, c)
		}
	}
	rows.Close()

	invoices, extended := 0, 0
	for _, c := range list {
		var inv *createdInvoice
		var wasFree bool
		err := func() error {
			tx, err := db.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback()
			// The list above is a snapshot. Lock the customer and look again: a staff member may
			// have assigned a plan, or another renewal run may have raised the invoice, in the
			// meantime.
			if err := lockUser(tx, c.id); err != nil {
				return err
			}
			var planID int
			var expiry, due time.Time
			err = tx.QueryRow(`
				SELECT ru.plan_id, ru.account_expiry, GREATEST(ru.account_expiry, CURRENT_DATE)
				FROM radius_users ru JOIN user_plans up ON up.id = ru.plan_id AND up.is_active
				WHERE ru.id = $1 AND ru.status = 'active' AND ru.account_expiry IS NOT NULL
				  AND ru.account_expiry BETWEEN CURRENT_DATE - 1 AND CURRENT_DATE + $2::int
				  AND NOT EXISTS (SELECT 1 FROM invoices i
				                  WHERE i.user_id = ru.id AND i.status IN ('pending','overdue'))`,
				c.id, days).Scan(&planID, &expiry, &due)
			if err == sql.ErrNoRows {
				return nil // no longer needs one
			}
			if err != nil {
				return err
			}
			p, err := loadPlan(tx, planID)
			if err != nil {
				return err
			}
			if p.Price <= 0 {
				if _, _, err := grantPlanPeriod(tx, c.id, p); err != nil {
					return err
				}
				wasFree = true
				return tx.Commit()
			}
			inv, err = createInvoiceTx(tx, newInvoice{UserID: &c.id, Username: c.name, PlanID: &p.ID,
				PlanName: &p.Name, Amount: p.Price, Currency: p.Currency, DueDate: &due, PlanDays: &p.ValidityDays,
				Notes: "Renewal for the period after " + expiry.Format("2006-01-02")})
			if err != nil {
				return err
			}
			return tx.Commit()
		}()
		if err != nil {
			log.WithError(err).Warnf("renewal for %s", c.name)
			continue
		}
		if wasFree {
			extended++
		}
		if inv != nil {
			invoices++
			go dispatchWebhookDB(db, "invoice.created", gin.H{"invoice_id": inv.ID, "invoice_number": inv.Number,
				"username": c.name, "amount": inv.Amount, "renewal": true})
		}
	}
	return fmt.Sprintf("%d renewal invoice(s) raised, %d free plan(s) extended", invoices, extended), invoices + extended
}

// sendExpiryReminders texts/emails customers whose paid-until date is within reminder_days,
// once per paid-until date (radius_users.last_reminder_for). Customers with no reachable
// channel are skipped and retried on the next run.
func sendExpiryReminders(db *database.DB, log *logrus.Logger) (string, int) {
	days := settings.Int("reminder_days", "", 3)
	sms, email := smsConfigured(), os.Getenv("SMTP_HOST") != ""
	if !sms && !email {
		return "no SMS gateway or SMTP configured; nothing sent", 0
	}
	tmpl := settings.String("reminder_message", "",
		"Dear {username}, your internet subscription expires on {expiry}. Please pay {amount} to stay connected.")
	org := settings.String("org_name", "", "")

	rows, err := db.Query(`
		SELECT ru.username, COALESCE(ru.phone,''), COALESCE(ru.email,''), ru.account_expiry,
		       COALESCE(up.name,''),
		       COALESCE((SELECT i.currency || ' ' || to_char(i.amount, 'FM999999990.00') FROM invoices i
		                 WHERE i.user_id = ru.id AND i.status IN ('pending','overdue') ORDER BY i.id DESC LIMIT 1),
		                up.currency || ' ' || to_char(up.price, 'FM999999990.00'), '')
		FROM radius_users ru
		LEFT JOIN user_plans up ON up.id = ru.plan_id
		WHERE ru.status = 'active' AND ru.account_expiry BETWEEN CURRENT_DATE AND CURRENT_DATE + $1::int
		  AND (ru.last_reminder_for IS NULL OR ru.last_reminder_for <> ru.account_expiry)`, days)
	if err != nil {
		return "expiry_reminders failed: " + err.Error(), 0
	}
	type cust struct {
		user, phone, mail, plan, amount string
		expiry                          time.Time
	}
	var list []cust
	for rows.Next() {
		var c cust
		if rows.Scan(&c.user, &c.phone, &c.mail, &c.expiry, &c.plan, &c.amount) == nil {
			list = append(list, c)
		}
	}
	rows.Close()

	sent := 0
	for _, c := range list {
		msg := strings.NewReplacer("{username}", c.user, "{expiry}", c.expiry.Format("2006-01-02"),
			"{amount}", c.amount, "{plan}", c.plan, "{org}", org).Replace(tmpl)
		ok := false
		if sms && c.phone != "" {
			ref, err := sendSMSGateway(c.phone, msg)
			status := "sent"
			if err != nil {
				status = "failed"
			} else {
				ok = true
			}
			logSMS(db, c.phone, msg, status, ref, c.user)
		}
		if email && c.mail != "" {
			if err := sendEmail(c.mail, "Your internet subscription expires on "+c.expiry.Format("2 Jan 2006"), msg); err == nil {
				ok = true
			} else {
				log.WithError(err).Warnf("reminder email to %s", c.user)
			}
		}
		if ok {
			db.Exec(`UPDATE radius_users SET last_reminder_for = account_expiry WHERE username = $1`, c.user)
			sent++
		}
	}
	return fmt.Sprintf("%d reminder(s) sent", sent), sent
}

func nextRunTime(schedule string) time.Time {
	switch schedule {
	case "hourly":
		return time.Now().Add(1 * time.Hour)
	case "weekly":
		return time.Now().AddDate(0, 0, 7)
	default: // daily
		return time.Now().AddDate(0, 0, 1)
	}
}
