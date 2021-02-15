// TODO: more expire/cleanup tests?

package gormstore

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// default test db
var dbURI = "file::memory:?cache=shared"

// TODO: this is ugly
func parseCookies(value string) map[string]*http.Cookie {
	m := map[string]*http.Cookie{}
	for _, c := range (&http.Request{Header: http.Header{"Cookie": {value}}}).Cookies() {
		m[c.Name] = c
	}
	return m
}

// create new shared in memory db
func newDB(uri string) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(uri), &gorm.Config{})
	if err != nil {
		panic(err)
	}

	// cleanup db
	if err := db.Migrator().DropTable("abc", "sessions"); err != nil {
		panic(err)
	}

	return db
}

func req(handler http.HandlerFunc, sessionCookie *http.Cookie) *httptest.ResponseRecorder {
	req, _ := http.NewRequest("GET", "http://test", nil)
	if sessionCookie != nil {
		req.Header.Add("Cookie", fmt.Sprintf("%s=%s", sessionCookie.Name, sessionCookie.Value))
	}
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

func match(t *testing.T, resp *httptest.ResponseRecorder, code int, body string) {
	if resp.Code != code {
		t.Errorf("Expected %v, actual %v", code, resp.Code)
	}
	// http.Error in countHandler adds a \n
	if strings.Trim(resp.Body.String(), "\n") != body {
		t.Errorf("Expected %v, actual %v", body, resp.Body)
	}
}

func findSession(db *gorm.DB, store *Store, id string) *gormSession {
	s := &gormSession{tableName: store.opts.TableName}
	if err := db.Where("id = ?", id).First(s).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	return s
}

func makeCountHandler(name string, store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, err := store.Get(r, name)
		if err != nil {
			panic(err)
		}

		count, _ := session.Values["count"].(int)
		count++
		session.Values["count"] = count
		if err := store.Save(r, w, session); err != nil {
			panic(err)
		}
		// leak session ID so we can mess with it in the db
		w.Header().Add("X-Session", session.ID)
		http.Error(w, fmt.Sprintf("%d", count), http.StatusOK)
	}
}

func TestBasic(t *testing.T) {
	countFn := makeCountHandler("session", New(newDB(dbURI), []byte("secret")))
	r1 := req(countFn, nil)
	match(t, r1, 200, "1")
	r2 := req(countFn, parseCookies(r1.Header().Get("Set-Cookie"))["session"])
	match(t, r2, 200, "2")
}

func TestExpire(t *testing.T) {
	db := newDB(dbURI)
	store := New(db, []byte("secret"))
	countFn := makeCountHandler("session", store)

	r1 := req(countFn, nil)
	match(t, r1, 200, "1")

	// test still in db but expired
	id := r1.Header().Get("X-Session")
	s := findSession(db, store, id)
	s.ExpiresAt = time.Now().Add(-40 * 24 * time.Hour)
	db.Save(s)

	r2 := req(countFn, parseCookies(r1.Header().Get("Set-Cookie"))["session"])
	match(t, r2, 200, "1")

	store.Cleanup()

	if findSession(db, store, id) != nil {
		t.Error("Expected session to be deleted")
	}
}

func TestBrokenCookie(t *testing.T) {
	db := newDB(dbURI)
	store := New(db, []byte("secret"))
	countFn := makeCountHandler("session", store)

	r1 := req(countFn, nil)
	match(t, r1, 200, "1")

	cookie := parseCookies(r1.Header().Get("Set-Cookie"))["session"]
	cookie.Value += "junk"
	r2 := req(countFn, cookie)
	match(t, r2, 200, "1")
}

func TestMaxAgeNegative(t *testing.T) {
	db := newDB(dbURI)
	store := New(db, []byte("secret"))
	countFn := makeCountHandler("session", store)

	r1 := req(countFn, nil)
	match(t, r1, 200, "1")

	r2 := req(func(w http.ResponseWriter, r *http.Request) {
		session, err := store.Get(r, "session")
		if err != nil {
			panic(err)
		}

		session.Options.MaxAge = -1
		store.Save(r, w, session)

		http.Error(w, "", http.StatusOK)
	}, parseCookies(r1.Header().Get("Set-Cookie"))["session"])

	match(t, r2, 200, "")
	c := parseCookies(r2.Header().Get("Set-Cookie"))["session"]
	if c.Value != "" {
		t.Error("Expected empty Set-Cookie session header", c)
	}

	id := r1.Header().Get("X-Session")
	if s := findSession(db, store, id); s != nil {
		t.Error("Expected session to be deleted")
	}
}

func TestMaxLength(t *testing.T) {
	store := New(newDB(dbURI), []byte("secret"))
	store.MaxLength(10)

	r1 := req(func(w http.ResponseWriter, r *http.Request) {
		session, err := store.Get(r, "session")
		if err != nil {
			panic(err)
		}

		session.Values["a"] = "aaaaaaaaaaaaaaaaaaaaaaaa"
		if err := store.Save(r, w, session); err == nil {
			t.Error("Expected too large error")
		}

		http.Error(w, "", http.StatusOK)
	}, nil)
	match(t, r1, 200, "")
}

func TestTableName(t *testing.T) {
	db := newDB(dbURI)
	store := NewOptions(db, Options{TableName: "abc"}, []byte("secret"))
	countFn := makeCountHandler("session", store)

	if !db.Migrator().HasTable(&gormSession{tableName: store.opts.TableName}) {
		t.Error("Expected abc table created")
	}

	r1 := req(countFn, nil)
	match(t, r1, 200, "1")
	r2 := req(countFn, parseCookies(r1.Header().Get("Set-Cookie"))["session"])
	match(t, r2, 200, "2")

	id := r2.Header().Get("X-Session")
	s := findSession(db, store, id)
	s.ExpiresAt = time.Now().Add(-time.Duration(store.SessionOpts.MaxAge+1) * time.Second)
	db.Save(s)

	store.Cleanup()

	if findSession(db, store, id) != nil {
		t.Error("Expected session to be deleted")
	}
}

func TestSkipCreateTable(t *testing.T) {
	db := newDB(dbURI)
	store := NewOptions(db, Options{SkipCreateTable: true}, []byte("secret"))

	if db.Migrator().HasTable(store.opts.TableName) {
		t.Error("Expected no table created")
	}
}

func TestMultiSessions(t *testing.T) {
	store := New(newDB(dbURI), []byte("secret"))
	countFn1 := makeCountHandler("session1", store)
	countFn2 := makeCountHandler("session2", store)

	r1 := req(countFn1, nil)
	match(t, r1, 200, "1")
	r2 := req(countFn2, nil)
	match(t, r2, 200, "1")

	r3 := req(countFn1, parseCookies(r1.Header().Get("Set-Cookie"))["session1"])
	match(t, r3, 200, "2")
	r4 := req(countFn2, parseCookies(r2.Header().Get("Set-Cookie"))["session2"])
	match(t, r4, 200, "2")
}

func TestPeriodicCleanup(t *testing.T) {
	db := newDB(dbURI)
	store := New(db, []byte("secret"))
	store.SessionOpts.MaxAge = 1
	countFn := makeCountHandler("session", store)

	quit := make(chan struct{})
	go store.PeriodicCleanup(200*time.Millisecond, quit)

	// test that cleanup i done at least twice

	r1 := req(countFn, nil)
	id1 := r1.Header().Get("X-Session")

	if findSession(db, store, id1) == nil {
		t.Error("Expected r1 session to exist")
	}

	time.Sleep(2 * time.Second)

	if findSession(db, store, id1) != nil {
		t.Error("Expected r1 session to be deleted")
	}

	r2 := req(countFn, nil)
	id2 := r2.Header().Get("X-Session")

	if findSession(db, store, id2) == nil {
		t.Error("Expected r2 session to exist")
	}

	time.Sleep(2 * time.Second)

	if findSession(db, store, id2) != nil {
		t.Error("Expected r2 session to be deleted")
	}

	close(quit)

	// test that cleanup has stopped

	r3 := req(countFn, nil)
	id3 := r3.Header().Get("X-Session")

	if findSession(db, store, id3) == nil {
		t.Error("Expected r3 session to exist")
	}

	time.Sleep(2 * time.Second)

	if findSession(db, store, id3) == nil {
		t.Error("Expected r3 session to exist")
	}
}

func TestMain(m *testing.M) {
	flag.Parse()

	if v := os.Getenv("DATABASE_URI"); v != "" {
		dbURI = v
	}
	fmt.Printf("DATABASE_URI=%s\n", dbURI)

	os.Exit(m.Run())
}
