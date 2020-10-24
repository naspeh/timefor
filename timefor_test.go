package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"testing"
	"text/template"

	"github.com/google/go-cmp/cmp"
	"github.com/jmoiron/sqlx"
	"gopkg.in/yaml.v2"
)

var db *sqlx.DB

func TestMain(m *testing.M) {
	err := exec.Command("sh", "-c", "go build").Run()
	if err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func TestSchema(t *testing.T) {
	db = sqlx.MustOpen("sqlite3", ":memory:")
	defer db.Close()
	initDb(db)

	err := Start(db, "test", 0)
	if err != nil {
		t.Fatal(err)
	}

	var count int
	_ = db.QueryRow(`SELECT count(*) FROM log`).Scan(&count)
	if count != 1 {
		t.Errorf("log table should have 1 row, but it has %v", count)
	}

	initDb(db)

	_ = db.QueryRow(`SELECT count(*) FROM log`).Scan(&count)
	if count != 1 {
		t.Errorf("log table should have 1 row, but it has %v", count)
	}

	err = Start(db, "test", 0)
	if diff := cmp.Diff(err.Error(), "Keep tracking existing activity"); diff != "" {
		t.Errorf("expected different error: %v", diff)
	}

	_, err = db.Exec("INSERT INTO log (name, started) VALUES ('test', strftime('%s', 'now'))")
	if err == nil {
		t.Error("insert should not succeed")
	}
	if diff := cmp.Diff(err.Error(), "UNIQUE constraint failed: log.current"); diff != "" {
		t.Errorf("expected different error: %v", diff)
	}

	_, err = db.Exec("INSERT INTO log (name, started, current) VALUES ('test', strftime('%s', 'now'), NULL)")
	if err == nil {
		t.Error("insert should not succeed")
	}
	if diff := cmp.Diff(err.Error(), "UNIQUE constraint failed: log.started"); diff != "" {
		t.Errorf("expected different error: %v", diff)
	}

	err = Start(db, "test2", 0)
	if diff := cmp.Diff(err.Error(), "cannot insert new activity into database: UNIQUE constraint failed: log.started"); diff != "" {
		t.Errorf("expected different error: %v", diff)
	}
}

func TestCmd(t *testing.T) {
	file, err := ioutil.TempFile("", "logtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(file.Name())

	db = sqlx.MustOpen("sqlite3", file.Name())
	defer db.Close()

	data, err := ioutil.ReadFile("testcmd.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var cases []struct {
		Name   string
		Cmd    string
		Code   int
		Output string
		Error  string
		Active bool
	}
	err = yaml.Unmarshal(data, &cases)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			line := fmt.Sprintf("DBFILE=%v ./timefor %v", file.Name(), c.Cmd)
			cmd := exec.Command("sh", "-c", line)
			out, err := cmd.CombinedOutput()
			var exiterr *exec.ExitError
			if c.Code == 0 && err != nil {
				t.Fatal(err)
			} else if errors.As(err, &exiterr) && exiterr.ExitCode() != c.Code {
				t.Errorf("expected code %v got %v", c.Code, exiterr.ExitCode())
			}
			latest, err := Latest(db)
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			err = template.Must(template.New("tpl").Parse(c.Output)).Execute(&buf, latest)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(strings.TrimSpace(string(out)), strings.TrimSpace(buf.String())); diff != "" {
				t.Logf("Got:\n%v", string(out))
				t.Errorf("expected different output: %v", diff)
			}
		})
	}
}
