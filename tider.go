package main

// /home/naspeh/.config/tider/log.db

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const timeoutForProlonging = 600

func main() {
	newCmd := flag.NewFlagSet("new", flag.ExitOnError)
	newShift := newCmd.String("shift", "", "start time shift like 10m, 1h10m, etc.")
	prolongCmd := flag.NewFlagSet("prolong", flag.ExitOnError)
	usage := fmt.Sprintf("expected %#v or %#v sub-command", newCmd.Name(), prolongCmd.Name())

	if len(os.Args) < 2 {
		fmt.Println(usage)
		os.Exit(1)
	}

	switch os.Args[1] {
	case newCmd.Name():
		_ = newCmd.Parse(os.Args[2:])
		fmt.Println(newCmd.Args())
		if len(newCmd.Args()) < 1 {
			fmt.Println("expected not empty name argument")
			os.Exit(1)
		}
		fmt.Println(newCmd.Args())
		NewActivity(newCmd.Args()[0], *newShift)
	case prolongCmd.Name():
		_ = prolongCmd.Parse(os.Args[2:])
		ProlongLastActivity()
	default:
		fmt.Println(usage)
		os.Exit(1)
	}
}

func connectDb() *sql.DB {
	db, err := sql.Open("sqlite3", "log.db")
	if err != nil {
		log.Fatalf("cannot open SQLite database: %v", err)
	}

	var name string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type="table" AND name="log"`).Scan(&name)
	switch err {
	case sql.ErrNoRows:
		initDb(db)
	case nil:
		log.Printf("SQLite database has been initialized already")
	default:
		log.Fatal(err)
	}
	fmt.Println(name, db)
	return db
}

func initDb(db *sql.DB) {
	_, err := db.Exec(`
		CREATE TABLE log(
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			start INTEGER NOT NULL,
			elapsed INTEGER NOT NULL DEFAULT 0,
			UNIQUE (name, start)
		);

		CREATE VIEW log_pretty AS
		SELECT
			id,
			name,
			date(start, 'unixepoch', 'localtime') start_date,
			time(start, 'unixepoch', 'localtime') start_time,
			elapsed,
			elapsed / 60 elapsed_minutes
		FROM log;
	`)
	if err != nil {
		log.Fatalf("cannot initiate SQLite database: %v", err)
	}
}

func NewActivity(name string, shift string) {
	db := connectDb()
	defer db.Close()

	shiftSeconds := 0
	if shift != "" {
		shiftDuration, err := time.ParseDuration(shift)
		if err != nil {
			log.Fatalf("wrong shift format: %v", err)
		}
		shiftSeconds = int(shiftDuration.Seconds())
	}
	_, err := db.Exec(
		`INSERT INTO log (name, start, elapsed) VALUES (?, strftime("%s","now") - ?, ?)`,
		name, shiftSeconds, shiftSeconds,
	)
	if err != nil {
		log.Fatalf("cannot insert new activity into database: %v", err)
	}
}

func ProlongLastActivity() {
	db := connectDb()
	defer db.Close()

	res, err := db.Exec(`
		WITH last_activity AS (
			SELECT id, name, start, strftime("%s","now") - start new_elapsed
			FROM log
			WHERE  strftime("%s","now") - (start + elapsed) < ? AND new_elapsed != elapsed
			ORDER BY id DESC
			LIMIT 1
		)
		UPDATE log SET (elapsed)=(SELECT new_elapsed FROM last_activity WHERE last_activity.id=log.id)
		WHERE id IN (SELECT id FROM last_activity)
	`, timeoutForProlonging)
	if err != nil {
		log.Fatalf("cannot update last activity: %v", err)
	}
	log.Printf("res: %v", res)
	rowCnt, err := res.RowsAffected()
	if err != nil {
		log.Fatalf("cannot update last activity: %v", err)
	}
	log.Printf("Count: %v", rowCnt)
	if rowCnt == 0 {
		log.Fatalf("no last activity")
	}
}
