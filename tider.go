package main

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strings"
	"text/template"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/cobra"
)

const (
	defaultDb           = "log.db"
	timeForExpire       = 10 * time.Minute
	sleepTimeForDaemon  = 30 * time.Second
	breakTimeForDaemon  = 80 * time.Minute
	repeatTimeForDaemon = 10 * time.Minute
	i3blocksTpl         = "{{.FormatDuration}} {{if .Active}}{{.Name}}\n\n#6666ee{{else}}OFF\n\n#666666{{end}}"
	defaultTpl          = "{{.FormatDuration}} {{if .Active}}{{.Name}}{{else}}OFF{{end}}"
)

func main() {
	dbFile := os.Getenv("DBFILE")
	if dbFile == "" {
		dbFile = defaultDb
	}
	db, err := sqlx.Open("sqlite3", dbFile)
	if err != nil {
		log.Fatalf("cannot open SQLite database: %v", err)
	}
	defer db.Close()

	initDb(db)

	err = newCmd(db).Execute()
	if err != nil {
		os.Exit(1)
	}
}

func newCmd(db *sqlx.DB) *cobra.Command {
	var newCmd = &cobra.Command{
		Use:   "new [activity name]",
		Short: "Create new activity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := cmd.Flags().GetString("name")
			if err != nil {
				return err
			}

			shift, err := cmd.Flags().GetDuration("shift")
			if err != nil {
				return err
			}
			if shift < 0 {
				return errors.New("shift cannot be negative")
			}

			rofi, err := cmd.Flags().GetBool("rofi")
			if err != nil {
				return err
			}
			if rofi {
				name = Select(db)
			}
			return New(db, name, shift)
		},
	}
	newCmd.Flags().StringP("name", "n", "", "activity name")
	newCmd.Flags().Duration("shift", 0, "start time shift (like 10m, 1m30s)")
	newCmd.Flags().Bool("rofi", false, "use rofi for name selection")

	var updateCmd = &cobra.Command{
		Use:   "update",
		Short: "Update the duration of the current activity (for cron use)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			Update(db, false)
			return nil
		},
	}

	var finishCmd = &cobra.Command{
		Use:   "finish",
		Short: "Finish the current activity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			Update(db, true)
			return nil
		},
	}

	var rejectCmd = &cobra.Command{
		Use:   "reject",
		Short: "Reject the current activity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			Reject(db)
			return nil
		},
	}

	var showCmd = &cobra.Command{
		Use:   "show",
		Short: "Show current activity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			tpl, err := cmd.Flags().GetString("template")
			if err != nil {
				return err
			}

			i3blocks, err := cmd.Flags().GetBool("i3blocks")
			if err != nil {
				return err
			}
			if i3blocks {
				tpl = i3blocksTpl
			}
			Show(db, tpl)
			return nil
		},
	}
	showCmd.Flags().Bool("i3blocks", false, "format for i3blocks")
	showCmd.Flags().StringP("template", "t", defaultTpl, "template for formatting")

	var daemonCmd = &cobra.Command{
		Use:   "daemon",
		Short: "Update the duration for the current activity in a loop",
		RunE: func(cmd *cobra.Command, args []string) error {
			sleepTime, err := cmd.Flags().GetDuration("sleep-time")
			if err != nil {
				return err
			}
			breakTime, err := cmd.Flags().GetDuration("break-time")
			if err != nil {
				return err
			}
			repeatTime, err := cmd.Flags().GetDuration("repeat-time")
			if err != nil {
				return err
			}
			Daemon(db, sleepTime, breakTime, repeatTime)
			return nil
		},
	}
	daemonCmd.Flags().Duration("sleep-time", sleepTimeForDaemon, "sleep time in the loop")
	daemonCmd.Flags().Duration("break-time", breakTimeForDaemon, "time for a break reminder")
	daemonCmd.Flags().Duration("repeat-time", repeatTimeForDaemon, "time to repeat a break reminder")

	var rootCmd = &cobra.Command{
		Use:   "tider",
		Short: "Simple time logger",
	}

	rootCmd.AddCommand(newCmd, updateCmd, finishCmd, rejectCmd, showCmd, daemonCmd)
	return rootCmd
}

func initDb(db *sqlx.DB) {
	var exists bool
	err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type="table" AND name="log"`).Scan(&exists)
	if err != nil {
		log.Fatal(err)
	} else if exists {
		return
	}
	_, err = db.Exec(`
		CREATE TABLE log(
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			started INTEGER UNIQUE NOT NULL,
			duration INTEGER NOT NULL DEFAULT 0,
			current INTEGER UNIQUE DEFAULT 1 CHECK (current IN (1))
		);

		CREATE TRIGGER on_insert_started INSERT ON log
		FOR EACH ROW
		BEGIN
			SELECT RAISE(ABORT, 'started must be latest')
			WHERE NEW.started < (SELECT MAX(started + duration) FROM log);
		END;

		CREATE VIEW latest AS
		SELECT id, name, started, duration, CASE WHEN current THEN 1 ELSE 0 END current
		FROM log
		ORDER BY started DESC
		LIMIT 1;

		CREATE VIEW log_pretty AS
		SELECT
			id,
			name,
			date(started, 'unixepoch', 'localtime') started_date,
			time(started, 'unixepoch', 'localtime') started_time,
			duration,
			duration / 60 duration_minutes,
			current,
			datetime(started + duration, 'unixepoch', 'localtime') updated
		FROM log;
	`)
	if err != nil {
		log.Fatalf("cannot initiate SQLite database: %v", err)
	}

	sql := fmt.Sprintf(`
		DROP VIEW IF EXISTS current;
		CREATE VIEW current AS
		SELECT *
		FROM log
		WHERE strftime('%%s', 'now') - started - duration < %v AND current
		ORDER BY id DESC
		LIMIT 1
	`, timeForExpire.Seconds())

	_, err = db.Exec(sql)
	if err != nil {
		log.Fatalf("cannot initiate SQLite database: %v", err)
	}
}

// Latest returns the latest activity if exists
func Latest(db *sqlx.DB) (activity Activity) {
	err := db.Get(&activity, `SELECT * FROM latest`)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatalf("cannot get the latest activity: %v", err)
	}
	return activity
}

// New creates new activity with name and optional time shift
func New(db *sqlx.DB, name string, shift time.Duration) error {
	name = strings.TrimSpace(name)
	activity := Latest(db)
	if activity.Active() && activity.Name == name {
		return errors.New("Keep tracking existing activity")
	}
	UpdateIfExists(db, true)
	_, err := db.NamedExec(`
		INSERT INTO log (name, started, duration) VALUES (:name, strftime('%s', 'now') - :shiftSeconds, :shiftSeconds)
	`, map[string]interface{}{
		"name":         name,
		"shiftSeconds": shift.Seconds(),
	})
	if err != nil {
		return fmt.Errorf("cannot insert new activity into database: %v", err)
	}
	return nil
}

// UpdateIfExists updates or finishes the current activity if exists
func UpdateIfExists(db *sqlx.DB, finish bool) bool {
	activity := Latest(db)
	if activity.Expired() {
		_ = db.MustExec(`
			UPDATE log SET current=NULL WHERE id = ?
		`, activity.ID)
		return false
	} else if !activity.Active() {
		return false
	}

	res, err := db.NamedExec(`
		UPDATE log SET
			duration=strftime('%s', 'now') - started,
			current=(CASE WHEN :shouldBeFinished THEN NULL ELSE 1 END)
		WHERE id IN (SELECT id FROM latest)
	`, map[string]interface{}{
		"shouldBeFinished": finish,
		"id":               activity.ID,
	})
	if err != nil {
		log.Fatalf("cannot update the current activity: %v", err)
	}
	rowCnt, err := res.RowsAffected()
	if err != nil {
		log.Fatalf("cannot update the current activity: %v", err)
	}
	return rowCnt != 0
}

// Update updates or finishes the current activity
func Update(db *sqlx.DB, finish bool) {
	updated := UpdateIfExists(db, finish)
	if !updated {
		log.Fatalf("no current activity")
	}
}

// Reject rejects the current activity (deletes it)
func Reject(db *sqlx.DB) {
	activity := Latest(db)
	_ = db.MustExec(`DELETE FROM log WHERE id = ?`, activity.ID)
}


// Show shows short information about the current activity
func Show(db *sqlx.DB, tpl string) {
	activity := Latest(db)
	fmt.Println(activity.Format(tpl))
}

// Daemon updates the duration of the current activity then sleeps for a while
func Daemon(db *sqlx.DB, sleepTime time.Duration, breakTime time.Duration, repeatTime time.Duration) {
	var notified time.Time
	for {
		UpdateIfExists(db, false)
		activity := Latest(db)
		if activity.Duration() > breakTime && time.Since(notified) > repeatTime {
			err := exec.Command("notify-send", "Take a break!").Run()
			if err != nil {
				log.Printf("cannot send notification: %v", err)
			}
			notified = time.Now()
		}
		time.Sleep(sleepTime)
	}
}
// Select selects new activity using rofi menu
func Select(db *sqlx.DB) string {
	var names []string
	err := db.Select(&names, `SELECT DISTINCT name FROM log ORDER BY started DESC`)
	if err != nil {
		log.Fatalf("cannot get names from SQLite database: %v", err)
	}
	cmd := exec.Command("rofi", "-dmenu")
	cmdIn, _ := cmd.StdinPipe()
	cmdOut, _ := cmd.StdoutPipe()
	_ = cmd.Start()
	for _, name := range names {
		fmt.Fprintln(cmdIn, name)
	}
	cmdIn.Close()
	selectedName, _ := ioutil.ReadAll(cmdOut)
	err = cmd.Wait()
	if err != nil {
		log.Fatalf("cannot get selection from rofi: %v", err)
	}
	return string(selectedName)
}

// Activity represents a named activity
type Activity struct {
	ID          int64
	Name        string
	StartedInt  int64 `db:"started"`
	DurationInt int64 `db:"duration"`
	Current     bool
}

func (a Activity) Format(tpl string) string {
	var buf bytes.Buffer
	err := template.Must(template.New("tpl").Parse(tpl)).Execute(&buf, a)
	if err != nil {
		log.Fatalf("cannot format activity: %v", err)
	}
	return strings.TrimSpace(buf.String())
}

func (a Activity) Started() time.Time {
	if a.StartedInt == 0 {
		return time.Now()
	}
	return time.Unix(a.StartedInt, 0)
}

func (a Activity) Duration() time.Duration {
	var duration time.Duration
	if a.Active() {
		duration = time.Since(a.Started())
	} else {
		duration = time.Since(a.Updated())
	}
	return duration.Truncate(time.Second)
}

func (a Activity) FormatDuration() string {
	d := a.Duration()
	d = d.Truncate(time.Minute)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	return fmt.Sprintf("%02d:%02d", h, m)
}

func (a Activity) Updated() time.Time {
	if a.StartedInt == 0 {
		return time.Now()
	}
	return time.Unix(a.StartedInt+a.DurationInt, 0)
}

func (a Activity) Expired() bool {
	return time.Since(a.Updated()) > timeForExpire
}

func (a Activity) Active() bool {
	return a.Current && !a.Expired()
}
