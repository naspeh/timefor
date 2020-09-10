package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
)

const timeoutForProlonging = 600
const timeForBreak = time.Duration(80) * time.Minute

func main() {
	newCmd := flag.NewFlagSet("new", flag.ExitOnError)
	newShift := newCmd.String("shift", "", "start time shift like 10m, 1h10m, etc.")
	newSelect := newCmd.Bool("select", false, "select name from rofi")

	updateCmd := flag.NewFlagSet("update", flag.ExitOnError)
	updateFinish := updateCmd.Bool("finish", false, "finish current activity")

	showCmd := flag.NewFlagSet("show", flag.ExitOnError)

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
			name = Select()
		} else {
			if len(newCmd.Args()) < 1 {
				log.Fatalln("expected not empty name argument")
			}
			name = newCmd.Args()[0]
		}
		New(name, *newShift)
	case updateCmd.Name():
		_ = updateCmd.Parse(os.Args[2:])
		Update(*updateFinish)
	case showCmd.Name():
		_ = showCmd.Parse(os.Args[2:])
		Show()
	case daemonCmd.Name():
		_ = daemonCmd.Parse(os.Args[2:])
		Daemon()
	default:
		log.Fatalln(usage)
	}
}

func connectDb() *sqlx.DB {
	db, err := sqlx.Open("sqlite3", "log.db")
	if err != nil {
		log.Fatalf("cannot open SQLite database: %v", err)
	}

	var exists bool
	err = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type="table" AND name="log"`).Scan(&exists)
	if err != nil {
		log.Fatal(err)
	} else if !exists {
		initDb(db)
	}
	return db
}

func initDb(db *sqlx.DB) {
	_, err := db.Exec(`
		CREATE TABLE log(
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			started INTEGER NOT NULL,
			duration INTEGER NOT NULL DEFAULT 0,
			updated INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
			UNIQUE (name, started)
		);

		CREATE TRIGGER on_insert_started INSERT ON log
		FOR EACH ROW
		BEGIN
			SELECT RAISE(ABORT, 'started must be latest')
			WHERE NEW.started < (SELECT MAX(started + duration) FROM log);
		END;

		CREATE TRIGGER on_insert_duration INSERT ON log
		FOR EACH ROW
		BEGIN
			SELECT RAISE(ABORT, 'must be only one current activity with duration=0')
			WHERE NEW.duration = 0 AND (SELECT count(*) FROM log WHERE duration=0);
		END;

		CREATE TRIGGER on_update_duration INSERT ON log
		FOR EACH ROW
		BEGIN
			SELECT RAISE(ABORT, 'must be only one current activity with duration=0')
			WHERE NEW.duration = 0 AND (SELECT count(*) FROM log WHERE duration=0 AND id != NEW.id);
		END;

		CREATE TRIGGER on_update_updated UPDATE ON log
		FOR EACH ROW
		BEGIN
			UPDATE log SET updated=strftime('%s', 'now') WHERE id = NEW.id;
		END;

		CREATE VIEW log_pretty AS
		SELECT
			id,
			name,
			date(started, 'unixepoch', 'localtime') start_date,
			time(started, 'unixepoch', 'localtime') start_time,
			duration,
			duration / 60 duration_minutes,
			datetime(updated, 'unixepoch', 'localtime') updated_ts
		FROM log;
	`)
	if err != nil {
		log.Fatalf("cannot initiate SQLite database: %v", err)
	}
	configure(db)
}

func configure(db *sqlx.DB) {
	sql := fmt.Sprintf(`
		DROP VIEW IF EXISTS current;
		CREATE VIEW current AS
		SELECT *, strftime('%%s', 'now') - started AS duration
		FROM log
		WHERE strftime('%%s', 'now') - updated < %v AND duration = 0
		ORDER BY id DESC
		LIMIT 1
	`, timeoutForProlonging)

	_, err := db.Exec(sql)
	if err != nil {
		log.Fatalf("cannot initiate SQLite database: %v", err)
	}
}

// New activity with name and optional time shift
func New(name string, shift string) {
	db := connectDb()
	defer db.Close()

	name = strings.TrimSpace(name)
	activity, err := Current(db)
	if err == nil && activity.Name == name {
		log.Printf("Keep tracking exisiting activity")
		return
	}
	UpdateIfExists(db, true)
	shiftSeconds := 0
	if shift != "" {
		shiftDuration, err := time.ParseDuration(shift)
		if err != nil {
			log.Fatalf("wrong shift format: %v", err)
		}
		shiftSeconds = int(shiftDuration.Seconds())
	}
	_, err = db.NamedExec(`
		INSERT INTO log (name, started) VALUES (:name, strftime('%s', 'now') - :shiftSeconds)
	`, map[string]interface{}{
		"name":         name,
		"shiftSeconds": shiftSeconds,
	})
	if err != nil {
		log.Fatalf("cannot insert new activity into database: %v", err)
	}
}

func UpdateIfExists(db *sqlx.DB, finish bool) bool {
	res, err := db.NamedExec(`
		UPDATE log SET
			duration=(CASE WHEN :shouldBeFinished THEN strftime('%s', 'now') - started ELSE 0 END),
			updated=strftime('%s', 'now')
		WHERE id IN (SELECT id FROM current)
	`, map[string]interface{}{
		"shouldBeFinished":     finish,
	})
	if err != nil {
		log.Fatalf("cannot update current activity: %v", err)
	}
	rowCnt, err := res.RowsAffected()
	if err != nil {
		log.Fatalf("cannot update current activity: %v", err)
	}
	if rowCnt == 0 {
		_, err := db.Exec(`
			UPDATE log SET duration = updated - started
			WHERE duration = 0
		`)
		if err != nil {
			log.Fatalf("cannot update current activity: %v", err)
		}
	}
	return rowCnt != 0
}

// Update or finish current activity
func Update(finish bool) {
	db := connectDb()
	defer db.Close()

	updated := UpdateIfExists(db, finish)
	if !updated {
		log.Fatalf("no current activity")
	}
}

// Select new activity from rofi
func Select() string {
	db := connectDb()
	defer db.Close()

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
	_ = cmd.Wait()
	return string(selectedName)
}

type Activity struct {
	Name     string
	Duration int
}

func fmtDuration(s int) string {
	d := time.Duration(s) * time.Second
	d = d.Round(time.Minute)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	return fmt.Sprintf("%02d:%02d", h, m)
}

func Current(db *sqlx.DB) (activity Activity, err error) {
	err = db.Get(&activity, `
		SELECT name, strftime('%s', 'now') - started AS duration
		FROM current
	`)
	return activity, err
}

// Show current activity
func Show() {
	db := connectDb()
	defer db.Close()

	var color, label string
	activity, err := Current(db)
	if err != nil {
		color, label = "#777777", fmt.Sprintf(" %v OFF", fmtDuration(0))
	} else {
		color, label = "#6666ee", fmt.Sprintf(" %v %v", fmtDuration(activity.Duration), activity.Name)
	}
	fmt.Printf("%v\n%v\n%v\n", label, label, color)
}

func Daemon() {
	db := connectDb()
	defer db.Close()

	for {
		activity, err := Current(db)
		if err == nil {
			UpdateIfExists(db, false)
		}
		if (time.Duration(activity.Duration) * time.Second) > timeForBreak {
			cmd := exec.Command("sh", "-c", `notify-send "Take a break!"`)
			_ = cmd.Run()
		}
		time.Sleep(1 * time.Minute)
	}
}
