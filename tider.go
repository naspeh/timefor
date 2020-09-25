package main

import (
	"bytes"
	"database/sql"
	"errors"
	"flag"
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
)

const timeoutForProlonging = 10 * time.Minute
const timeForBreak = 80 * time.Minute

func main() {
	db, err := sqlx.Open("sqlite3", "log.db")
	if err != nil {
		log.Fatalf("cannot open SQLite database: %v", err)
	}
	defer db.Close()

	initDb(db)

	newCmd := flag.NewFlagSet("new", flag.ExitOnError)
	newShift := newCmd.String("shift", "", "start time shift like 10m, 1h10m, etc.")
	newSelect := newCmd.Bool("select", false, "select name from rofi")

	updateCmd := flag.NewFlagSet("update", flag.ExitOnError)
	updateFinish := updateCmd.Bool("finish", false, "finish the current activity")

	showCmd := flag.NewFlagSet("show", flag.ExitOnError)
	showFmt := showCmd.String("format", "default", "select format")

	daemonCmd := flag.NewFlagSet("daemon", flag.ExitOnError)

	usage := fmt.Sprintf(
		"expected sub-command: %v",
		[]string{newCmd.Name(), updateCmd.Name(), showCmd.Name(), daemonCmd.Name()},
	)

	if len(os.Args) < 2 {
		log.Fatalln(usage)
	}

	switch os.Args[1] {
	case newCmd.Name():
		_ = newCmd.Parse(os.Args[2:])
		var name string
		if *newSelect {
			name = Select(db)
		} else {
			if len(newCmd.Args()) < 1 {
				log.Fatalln("expected not empty name argument")
			}
			name = newCmd.Args()[0]
		}
		New(db, name, *newShift)
	case updateCmd.Name():
		_ = updateCmd.Parse(os.Args[2:])
		Update(db, *updateFinish)
	case showCmd.Name():
		_ = showCmd.Parse(os.Args[2:])
		Show(db, *showFmt)
	case daemonCmd.Name():
		_ = daemonCmd.Parse(os.Args[2:])
		Daemon(db)
	default:
		log.Fatalln(usage)
	}
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
	`, timeoutForProlonging.Seconds())

	_, err = db.Exec(sql)
	if err != nil {
		log.Fatalf("cannot initiate SQLite database: %v", err)
	}
}

// New creates new activity with name and optional time shift
func New(db *sqlx.DB, name string, shift string) {
	name = strings.TrimSpace(name)
	activity := Latest(db)
	if activity.Active() && activity.Name == name {
		log.Printf("Keep tracking exisiting activity")
		return
	}
	UpdateIfExists(db, true)
	shiftSeconds := 0.0
	if shift != "" {
		shiftDuration, err := time.ParseDuration(shift)
		if err != nil {
			log.Fatalf("wrong shift format: %v", err)
		}
		shiftSeconds = shiftDuration.Seconds()
	}
	_, err := db.NamedExec(`
		INSERT INTO log (name, started, duration) VALUES (:name, strftime('%s', 'now') - :shiftSeconds, :shiftSeconds)
	`, map[string]interface{}{
		"name":         name,
		"shiftSeconds": shiftSeconds,
	})
	if err != nil {
		log.Fatalf("cannot insert new activity into database: %v", err)
	}
}

// UpdateIfExists updates or finishes the current activity if exists
func UpdateIfExists(db *sqlx.DB, finish bool) bool {
	activity := Latest(db)
	if activity.Expired() {
		_ = db.MustExec(`
			UPDATE log SET current=NULL WHERE id = ?
		`, activity.Id)
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
		"id":               activity.Id,
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

const i3blocksTpl = `
{{if .Active}}
 {{.FormatDuration}} {{.Name}}

#6666ee
{{else}}
 {{.FormatDuration}}  OFF

#777777
{{end}}
`
const defaultTpl = `
{{if .Active}}
 {{.FormatDuration}} {{.Name}}
{{else}}
 {{.FormatDuration}}  OFF
{{end}}
`

var Templates = map[string]string{
	"default":  defaultTpl,
	"i3blocks": i3blocksTpl,
}

// Activity represents a named activity
type Activity struct {
	Id          int64
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
	if a.Active() {
		return time.Since(a.Started())
	}
	return time.Since(a.Updated())
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
	return time.Since(a.Updated()) > timeoutForProlonging
}

func (a Activity) Active() bool {
	return a.Current && !a.Expired()
}

// Latest returns the latest activity if exists
func Latest(db *sqlx.DB) (activity Activity) {
	err := db.Get(&activity, `SELECT * FROM latest`)
	if errors.Is(err, sql.ErrNoRows) {
		log.Fatalf("cannot get the latest activity: %v", err)
	}
	return activity
}

// Show shows short information about the current activity
func Show(db *sqlx.DB, tpl string) {
	tpl = Templates[tpl]
	activity := Latest(db)
	fmt.Println(activity.Format(tpl))
}

// Daemon updates the duration of the current activity then sleeps for a while
func Daemon(db *sqlx.DB) {
	for {
		UpdateIfExists(db, false)
		activity := Latest(db)
		if activity.Duration() > timeForBreak {
			cmd := exec.Command("sh", "-c", `notify-send "Take a break!"`)
			_ = cmd.Run()
		}
		time.Sleep(1 * time.Minute)
	}
}
