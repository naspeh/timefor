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
	timeForExpire       = 10 * time.Minute
	sleepTimeForDaemon  = 30 * time.Second
	breakTimeForDaemon  = 80 * time.Minute
	repeatTimeForDaemon = 10 * time.Minute
	i3blocksTpl         = "{{.FormatTimeSince}} {{if .Active}}{{.Name}}\n\n#6666ee{{else}}OFF\n\n#666666{{end}}"
	defaultTpl          = "{{.FormatTimeSince}} {{if .Active}}{{.Name}}{{else}}OFF{{end}}"
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

	initDb(db)

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
			name := Select(db)
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

	var reportCmd = &cobra.Command{
		Use:   "report",
		Short: "Report today's activities",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			notify, err := cmd.Flags().GetBool("notify")
			if err != nil {
				return err
			}
			title, desc := Report(db)
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
		Short: "Update the duration for current activity in a loop",
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
		Use:   "timefor",
		Short: "A command-line time tracker with rofi integration",
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
	`)
	if err != nil {
		log.Fatalf("cannot initiate SQLite database: %v", err)
	}
	initDbViews(db)
}

func initDbViews(db *sqlx.DB) {
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

// Start starts new activity
func Start(db *sqlx.DB, name string, shift time.Duration) error {
	name = strings.TrimSpace(name)
	activity := Latest(db)
	if activity.Active() && activity.Name == name {
		return errors.New("Keep tracking existing activity")
	}
	UpdateIfExists(db, "", true)
	_, err := db.NamedExec(`
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
func UpdateIfExists(db *sqlx.DB, name string, finish bool) bool {
	activity := Latest(db)
	if activity.Expired() {
		_ = db.MustExec(`
			UPDATE log SET current=NULL WHERE id = ?
		`, activity.ID)
		return false
	} else if !activity.Active() {
		return false
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
		log.Fatalf("cannot update current activity: %v", err)
	}
	rowCnt, err := res.RowsAffected()
	if err != nil {
		log.Fatalf("cannot update current activity: %v", err)
	}
	return rowCnt != 0
}

// Update updates or finishes current activity
func Update(db *sqlx.DB, name string, finish bool) error {
	updated := UpdateIfExists(db, name, finish)
	if !updated {
		return errors.New("no current activity")
	}
	return nil
}

// Reject rejects current activity (deletes it)
func Reject(db *sqlx.DB) error {
	activity := Latest(db)
	if activity.Active() {
		_, err := db.Exec(`DELETE FROM log WHERE id = ?`, activity.ID)
		if err != nil {
			return err
		}
	}
	return nil
}

// Show shows short information about current activity
func Show(db *sqlx.DB, tpl string) {
	activity := Latest(db)
	fmt.Println(activity.Format(tpl))
}

// Daemon updates the duration of current activity then sleeps for a while
func Daemon(db *sqlx.DB, sleepTime time.Duration, breakTime time.Duration, repeatTime time.Duration) {
	var notified time.Time
	for {
		UpdateIfExists(db, "", false)
		activity := Latest(db)
		duration := activeDuration(db)
		if activity.Active() && duration > breakTime && time.Since(notified) > repeatTime {
			args := []string{
				"Take a break!",
				fmt.Sprintf("Active for %v already", formatDuration(duration)),
			}
			if duration.Seconds() > breakTime.Seconds()*1.2 {
				args = append(args, "-u", "critical")
			} else {
				args = append(args, "-t", "3")
			}
			err := exec.Command("notify-send", args...).Run()
			if err != nil {
				log.Printf("cannot send notification: %v", err)
			}
			notified = time.Now()
		}
		time.Sleep(sleepTime)
	}
}

func activeDuration(db *sqlx.DB) time.Duration {
	rows, err := db.Queryx(`SELECT * FROM log ORDER BY started DESC LIMIT 100`)
	if err != nil {
		panic(err)
	}
	defer rows.Close()

	duration := time.Duration(0)
	cur := Activity{}
	prev := Activity{}
	for rows.Next() {
		err := rows.StructScan(&cur)
		if err != nil {
			panic(err)
		}
		if prev.ID == 0 && cur.Expired() {
			break
		} else if prev.Started().Sub(cur.Updated()) > timeForExpire {
			break
		}
		duration += cur.Duration()
		prev = cur
	}
	err = rows.Err()
	if err != nil {
		panic(err)
	}
	return duration
}

// Report reports about today's activities for now
// TODO: add custom time range support
func Report(db *sqlx.DB) (title, desc string) {
	duration := activeDuration(db)
	if duration == time.Duration(0) {
		latest := Latest(db)
		title = fmt.Sprintf("Not active for %v ", latest.FormatTimeSince())
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
		panic(err)
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
			panic(err)
		}
		count += 1
		duration += a.Duration()
		fmt.Fprintf(tabw, lineTpl, a.Name, formatDuration(a.Duration()))
		if len(a.Name) > maxLength {
			maxLength = len(a.Name)
		}
	}
	if count == 0 {
		return title, ""
	}
	if count > 1 {
		fmt.Fprintf(tabw, lineTpl, strings.Repeat("-", maxLength), "-----")
		fmt.Fprintf(tabw, lineTpl, "Total", formatDuration(duration))
	}
	tabw.Flush()
	return title, buf.String()
}

func formatDuration(d time.Duration) string {
	d = d.Truncate(time.Minute)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	return fmt.Sprintf("%02d:%02d", h, m)
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
	Current     sql.NullBool
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
	return a.Current.Bool && !a.Expired()
}
