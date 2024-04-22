package main

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path"
	"strings"
	"text/tabwriter"
	"text/template"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/cobra"
)

const (
	intervalToExpire                     = 10 * time.Minute
	defaultIntervalToUpdateDb            = 30 * time.Second
	defaultIntervalToShowBreakReminder   = 80 * time.Minute
	defaultIntervalToRepeatBreakReminder = 10 * time.Minute
	defaultTpl                           = "{{if .Active}}☭{{else}}☯{{end}} {{.FormatLabel}}"
)

var dbFile string

func main() {
	dbFile = os.Getenv("DBFILE")
	if dbFile == "" {
		usr, err := user.Current()
		if err != nil {
			log.Fatalf("cannot get current user: %v", err)
		}
		dbFile = path.Join(usr.HomeDir, ".timefor.db")
	}
	db, err := sqlx.Open("sqlite3", dbFile)
	if err != nil {
		log.Fatalf("cannot open SQLite database: %v", err)
	}
	defer db.Close()

	err = initDb(db)
	if err != nil {
		log.Fatalf("cannot initiate SQLite database: %v", err)
	}

	err = newCmd(db).Execute()
	if err != nil {
		os.Exit(1)
	}
}

func newCmd(db *sqlx.DB) *cobra.Command {
	var startCmd = &cobra.Command{
		Use:   "start [activity name]",
		Short: "Start new activity",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			shift, err := cmd.Flags().GetDuration("shift")
			if err != nil {
				return err
			}
			if shift < 0 {
				return errors.New("shift cannot be negative")
			}
			return Start(db, args[0], shift)
		},
	}
	startCmd.Flags().Duration("shift", 0, "start time shift (like 10m, 1m30s)")

	var selectCmd = &cobra.Command{
		Use:   "select",
		Short: "Select new activity using rofi",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			update, err := cmd.Flags().GetBool("update")
			if err != nil {
				return err
			}
			name, err := Select(db)
			if err != nil {
				return err
			}
			if update {
				return Update(db, name, false)
			}
			return Start(db, name, 0)
		},
	}
	selectCmd.Flags().Bool("update", false, "update current activity instead")

	var updateCmd = &cobra.Command{
		Use:   "update",
		Short: "Update the duration of current activity (for cron use)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := cmd.Flags().GetString("name")
			if err != nil {
				return err
			}
			return Update(db, name, false)
		},
	}
	updateCmd.Flags().String("name", "", "change the name as well")

	var finishCmd = &cobra.Command{
		Use:   "finish",
		Short: "Finish current activity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return Update(db, "", true)
		},
	}

	var rejectCmd = &cobra.Command{
		Use:   "reject",
		Short: "Reject current activity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return Reject(db)
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
			return Show(db, tpl)
		},
	}
	showCmd.Flags().StringP("template", "t", defaultTpl, "template for formatting")

	var reportCmd = &cobra.Command{
		Use:   "report",
		Short: "Report today's activities",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			notify, err := cmd.Flags().GetBool("notify")
			if err != nil {
				return err
			}
			title, desc, err := Report(db)
			if err != nil {
				return err
			}
			if notify {
				args = []string{"-t", "0", title, desc}
				err := exec.Command("notify-send", args...).Run()
				if err != nil {
					log.Printf("cannot send notification: %v", err)
				}
				return nil
			}
			fmt.Printf("%v\n\n", title)
			fmt.Println(desc)
			return nil
		},
	}
	reportCmd.Flags().BoolP("notify", "n", false, "Notify using notify-send")

	var daemonCmd = &cobra.Command{
		Use:   "daemon",
		Short: "Update the duration for current activity and run hook if specified",
		RunE: func(cmd *cobra.Command, args []string) error {
			intervalToUpdateDb, err := cmd.Flags().GetDuration("update-interval")
			if err != nil {
				return err
			}
			intervalToShowBreakReminder, err := cmd.Flags().GetDuration("break-interval")
			if err != nil {
				return err
			}
			intervalToRepeatBreakReminder, err := cmd.Flags().GetDuration("repeat-interval")
			if err != nil {
				return err
			}
			hook, err := cmd.Flags().GetString("hook")
			if err != nil {
				return err
			}
			err = Daemon(db, intervalToUpdateDb, intervalToShowBreakReminder, intervalToRepeatBreakReminder, hook)
			if err != nil {
				return err
			}
			return nil
		},
	}
	daemonCmd.Flags().Duration("update-interval", defaultIntervalToUpdateDb, "interval to update activity time in db")
	daemonCmd.Flags().Duration("break-interval", defaultIntervalToShowBreakReminder, "interval to show a break reminder")
	daemonCmd.Flags().Duration("repeat-interval", defaultIntervalToRepeatBreakReminder, "interval to repeat a break reminder")
	daemonCmd.Flags().StringP("hook", "", "", "a hook command template")

	var dbCmd = &cobra.Command{
		Use:   "db",
		Short: "Execute sqlite3 with db file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dbviews, err := cmd.Flags().GetBool("update-views")
			if err != nil {
				return err
			}
			if dbviews {
				initDbViews(db)
				return nil
			}
			c := exec.Command("sqlite3", "-box", dbFile)
			c.Stdin = os.Stdin
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			return c.Run()
		},
	}
	dbCmd.Flags().Bool("update-views", false, "update sqlite views and exit")

	var rootCmd = &cobra.Command{
		Use:          "timefor",
		Short:        "A command-line time tracker with rofi integration",
		SilenceUsage: true,
	}

	rootCmd.AddCommand(
		startCmd,
		selectCmd,
		updateCmd,
		finishCmd,
		rejectCmd,
		showCmd,
		reportCmd,
		dbCmd,
		daemonCmd,
	)
	return rootCmd
}

func initDb(db *sqlx.DB) error {
	var exists bool
	err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type="table" AND name="log"`).Scan(&exists)
	if err != nil {
		return err
	} else if exists {
		return nil
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
	`)
	if err != nil {
		return err
	}
	return initDbViews(db)
}

func initDbViews(db *sqlx.DB) error {
	_, err := db.Exec(`
		DROP VIEW IF EXISTS latest;
		CREATE VIEW latest AS
		SELECT *
		FROM log
		ORDER BY started DESC
		LIMIT 1;

		DROP VIEW IF EXISTS log_pretty;
		CREATE VIEW log_pretty AS
		SELECT
			id,
			name,
			date(started, 'unixepoch', 'localtime') started_date,
			time(started, 'unixepoch', 'localtime') started_time,
			duration,
			time(duration, 'unixepoch') duration_pretty,
			current,
			datetime(started + duration, 'unixepoch', 'localtime') updated
		FROM log;

		DROP VIEW IF EXISTS log_daily;
		CREATE VIEW log_daily AS
		SELECT
			started_date as date,
			name,
			time(SUM(duration), 'unixepoch') duration_pretty,
			SUM(duration) duration
		FROM log_pretty
		GROUP BY started_date, name;

		-- Drop deprecated views
		DROP VIEW IF EXISTS current;
	`)
	if err != nil {
		return err
	}
	return nil
}

// Latest returns the latest activity if exists
func Latest(db *sqlx.DB) (activity Activity, err error) {
	err = db.Get(&activity, `SELECT * FROM latest`)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Activity{}, fmt.Errorf("cannot get the latest activity: %v", err)
	}
	return activity, nil
}

// Start starts new activity
func Start(db *sqlx.DB, name string, shift time.Duration) error {
	name = strings.TrimSpace(name)
	activity, err := Latest(db)
	if err != nil {
		return err
	}
	if activity.Active() && activity.Name == name {
		return errors.New("Keep tracking existing activity")
	}
	_, err = UpdateIfExists(db, "", true)
	if err != nil {
		return err
	}
	_, err = db.NamedExec(`
		INSERT INTO log (name, started, duration) VALUES (:name, strftime('%s', 'now') - :shiftSeconds, :shiftSeconds)
	`, map[string]interface{}{
		"name":         name,
		"shiftSeconds": shift.Seconds(),
	})
	if err != nil {
		return fmt.Errorf("cannot insert new activity into database: %v", err)
	}
	fmt.Printf("New activity %#v started\n", name)
	return nil
}

// UpdateIfExists updates or finishes current activity if exists
func UpdateIfExists(db *sqlx.DB, name string, finish bool) (bool, error) {
	activity, err := Latest(db)
	if err != nil {
		return false, err
	}
	if activity.Expired() {
		_, err := db.Exec(`UPDATE log SET current=NULL WHERE id = ?`, activity.ID)
		if err != nil {
			return false, err
		}
		return false, nil
	} else if !activity.Active() {
		return false, nil
	}

	name = strings.TrimSpace(name)
	if name == "" {
		name = activity.Name
	}

	res, err := db.NamedExec(`
		UPDATE log SET
			duration=strftime('%s', 'now') - started,
			current=(CASE WHEN :shouldBeFinished THEN NULL ELSE 1 END),
			name=:name
		WHERE id IN (SELECT id FROM latest)
	`, map[string]interface{}{
		"shouldBeFinished": finish,
		"name":             name,
		"id":               activity.ID,
	})
	if err != nil {
		return false, err
	}
	rowCnt, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rowCnt != 0, nil
}

// Update updates or finishes current activity
func Update(db *sqlx.DB, name string, finish bool) error {
	updated, err := UpdateIfExists(db, name, finish)
	if err != nil {
		return err
	}
	if !updated {
		return errors.New("no current activity")
	}
	return nil
}

// Reject rejects current activity (deletes it)
func Reject(db *sqlx.DB) error {
	activity, err := Latest(db)
	if err != nil {
		return err
	}
	if activity.Active() {
		_, err := db.Exec(`DELETE FROM log WHERE id = ?`, activity.ID)
		if err != nil {
			return err
		}
	}
	return nil
}

// Show shows short information about current activity
func Show(db *sqlx.DB, tpl string) error {
	activity, err := Latest(db)
	if err != nil {
		return err
	}
	txt, err := activity.Format(tpl)
	if err != nil {
		return err
	}
	fmt.Println(txt)
	return nil
}

// Daemon updates the duration of current activity and runs the hook if specified
func Daemon(
	db *sqlx.DB,
	intervalToUpdateDb time.Duration,
	intervalToShowBreakReminder time.Duration,
	intervalToRepeatBreakReminder time.Duration,
	hook string,
) error {
	var notified time.Time
	var lastHook string
	for {
		activity, err := Latest(db)
		if err != nil {
			return err
		}
		if hook != "" {
			cmd, err := activity.Format(hook)
			if err != nil {
				return fmt.Errorf("cannot render hook command: %v", err)
			}
			if lastHook != cmd {
				lastHook = cmd
				fmt.Printf("running hook command: %s\n", cmd)
				err = exec.Command("sh", "-c", cmd).Run()
				if err != nil {
					return fmt.Errorf("cannot run hook command: %v", err)
				}
			}
		}
		if activity.Active() && time.Since(activity.Updated()) > intervalToUpdateDb {
			fmt.Printf("updating time for %s\n", activity.Name)
			_, err := UpdateIfExists(db, "", false)
			if err != nil {
				return err
			}
			duration, err := activeDuration(db)
			if err != nil {
				return err
			}
			if activity.Active() && duration > intervalToShowBreakReminder && time.Since(notified) > intervalToRepeatBreakReminder {
				args := []string{
					"Take a break!",
					fmt.Sprintf("Active for %v already", formatDuration(duration)),
				}
				if duration.Seconds() > intervalToShowBreakReminder.Seconds()*1.2 {
					args = append(args, "-u", "critical")
				} else {
					// default timeout is too quick, so set it to 5s
					args = append(args, "-t", "5000")
				}
				err := exec.Command("notify-send", args...).Run()
				if err != nil {
					fmt.Printf("cannot send notification: %v", err)
				}
				notified = time.Now()
			}
		}
		time.Sleep(1 * time.Second)
	}
}

func activeDuration(db *sqlx.DB) (time.Duration, error) {
	rows, err := db.Queryx(`SELECT * FROM log ORDER BY started DESC LIMIT 100`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	duration := time.Duration(0)
	cur := Activity{}
	prev := Activity{}
	for rows.Next() {
		err := rows.StructScan(&cur)
		if err != nil {
			return 0, err
		}
		if prev.ID == 0 && cur.Expired() {
			break
		} else if prev.Started().Sub(cur.Updated()) > intervalToExpire {
			break
		}
		duration += cur.Duration()
		prev = cur
	}
	err = rows.Err()
	if err != nil {
		return 0, err
	}
	return duration, nil
}

// Report reports about today's activities for now
// TODO: add custom time range support
func Report(db *sqlx.DB) (title, desc string, err error) {
	duration, err := activeDuration(db)
	if err != nil {
		return "", "", err
	}
	if duration == time.Duration(0) {
		latest, err := Latest(db)
		if err != nil {
			return "", "", err
		}
		title = fmt.Sprintf("Inactive for %v ", latest.FormatTimeSince())
	} else {
		title = fmt.Sprintf("Active for %v", formatDuration(duration))
	}

	rows, err := db.Queryx(`
		SELECT name, duration
		FROM log_daily
		WHERE date = date('now')
		GROUP BY name;
	`)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()

	buf := bytes.Buffer{}
	tabw := tabwriter.NewWriter(&buf, 0, 0, 1, ' ', tabwriter.TabIndent)
	lineTpl := "%v\t %v\n"

	duration = time.Duration(0)
	a := Activity{}
	count := 0
	maxLength := 5 // length of "Total"
	for rows.Next() {
		err := rows.StructScan(&a)
		if err != nil {
			return "", "", err
		}
		count += 1
		duration += a.Duration()
		fmt.Fprintf(tabw, lineTpl, a.Name, formatDuration(a.Duration()))
		if len(a.Name) > maxLength {
			maxLength = len(a.Name)
		}
	}
	if count == 0 {
		return title, "", nil
	}
	if count > 1 {
		fmt.Fprintf(tabw, lineTpl, strings.Repeat("-", maxLength), "-----")
		fmt.Fprintf(tabw, lineTpl, "Total", formatDuration(duration))
	}
	tabw.Flush()
	return title, buf.String(), nil
}

func formatDuration(d time.Duration) string {
	d = d.Truncate(time.Minute)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	return fmt.Sprintf("%02d:%02d", h, m)
}

// Select selects new activity using rofi menu
func Select(db *sqlx.DB) (string, error) {
	var names []string
	err := db.Select(&names, `SELECT DISTINCT name FROM log ORDER BY started DESC`)
	if err != nil {
		return "", err
	}
	cmd := exec.Command("rofi", "-dmenu")

	cmdIn, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}

	cmdOut, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}

	err = cmd.Start()
	if err != nil {
		return "", err
	}
	for _, name := range names {
		fmt.Fprintln(cmdIn, name)
	}
	cmdIn.Close()
	selectedName, err := io.ReadAll(cmdOut)
	if err != nil {
		return "", err
	}
	err = cmd.Wait()
	if err != nil {
		return "", fmt.Errorf("cannot get selection from rofi: %v", err)
	}
	return string(selectedName), nil
}

// Activity represents a named activity
type Activity struct {
	ID          int64
	Name        string
	StartedInt  int64 `db:"started"`
	DurationInt int64 `db:"duration"`
	Current     sql.NullBool
}

func (a Activity) Format(tpl string) (string, error) {
	var buf bytes.Buffer
	t, err := template.New("tpl").Parse(tpl)
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %v", err)
	}
	err = t.Execute(&buf, a)
	if err != nil {
		return "", fmt.Errorf("cannot format activity: %v", err)
	}
	return strings.TrimSpace(buf.String()), nil
}

func (a Activity) Started() time.Time {
	if a.StartedInt == 0 {
		return time.Now()
	}
	return time.Unix(a.StartedInt, 0)
}

func (a Activity) TimeSince() time.Duration {
	var duration time.Duration
	if a.Active() {
		duration = time.Since(a.Started())
	} else {
		duration = time.Since(a.Updated())
	}
	return duration.Truncate(time.Second)
}

func (a Activity) Duration() time.Duration {
	var duration time.Duration
	if a.Active() {
		duration = time.Since(a.Started())
	} else {
		duration = time.Duration(a.DurationInt) * time.Second
	}
	return duration.Truncate(time.Second)
}

func (a Activity) FormatTimeSince() string {
	return formatDuration(a.TimeSince())
}

func (a Activity) FormatLabel() string {
	name := a.Name
	if !a.Active() {
		name = "OFF"
	}
	return fmt.Sprintf("%s %s", a.FormatTimeSince(), name)
}

func (a Activity) Updated() time.Time {
	if a.StartedInt == 0 {
		return time.Now()
	}
	return time.Unix(a.StartedInt+a.DurationInt, 0)
}

func (a Activity) Expired() bool {
	return time.Since(a.Updated()) > intervalToExpire
}

func (a Activity) Active() bool {
	return a.Current.Bool && !a.Expired()
}
