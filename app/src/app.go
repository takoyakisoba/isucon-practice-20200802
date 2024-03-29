package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"contrib.go.opencensus.io/integrations/ocsql"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gomarkdown/markdown"
	"github.com/gorilla/mux"
	"github.com/gorilla/securecookie"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
)

const (
	memosPerPage   = 100
	listenAddr     = ":5000"
	sessionName    = "isucon_session"
	dbConnPoolSize = 10
	sessionSecret  = "kH<{11qpic*gf0e21YK7YtwyUvE9l<1r>yX8R-Op"
)

type Config struct {
	Database struct {
		Dbname   string `json:"dbname"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"database"`
}

type User struct {
	Id         int
	Username   string
	Password   string
	Salt       string
	LastAccess string
}

type Memo struct {
	Id        int
	User      int
	Title string
	Content   string
	IsPrivate int
	CreatedAt string
	UpdatedAt string
	Username  string
}

type Memos []*Memo

type View struct {
	User      *User
	Memo      *Memo
	Memos     *Memos
	Page      int
	PageStart int
	PageEnd   int
	Total     int
	Older     *Memo
	Newer     *Memo
	Session   *sessions.Session
}

var (
	dbConnPool chan *sqlx.DB
	baseUrl    *url.URL
	fmap       = template.FuncMap{
		"url_for": func(path string) string {
			return baseUrl.String() + path
		},
		"first_line": func(s string) string {
			sl := strings.Split(s, "\n")
			return sl[0]
		},
		"get_token": func(session *sessions.Session) interface{} {
			return session.Values["token"]
		},
		"gen_markdown": func(s string) template.HTML {
			html := markdown.ToHTML([]byte(s), nil, nil)
			return template.HTML(string(html))
		},
	}
	tmpl = template.Must(template.New("tmpl").Funcs(fmap).ParseGlob("templates/*.html"))
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	env := os.Getenv("ISUCON_ENV")
	if env == "" {
		env = "local"
	}
	if env != "local" {
		initProfiler()
		initTrace()
	}
	config := loadConfig("../config/" + env + ".json")
	db := config.Database
	connectionString := fmt.Sprintf(
		"%s:%s@tcp(%s:%d)/%s?charset=utf8",
		db.Username, db.Password, db.Host, db.Port, db.Dbname,
	)
	log.Printf("db: %s", connectionString)

	dbConnPool = make(chan *sqlx.DB, dbConnPoolSize)
	for i := 0; i < dbConnPoolSize; i++ {
		conn, err := sql.Open(tracedDriver("mysql"), connectionString)
		if err != nil {
			log.Panicf("Error opening database: %v", err)
		}

		dbx := sqlx.NewDb(conn, "mysql")
		dbConnPool <- dbx
		defer ocsql.RecordStats(conn, 5*time.Second)()
		defer conn.Close()
	}

	r := mux.NewRouter()
	r.HandleFunc("/", topHandler)
	r.HandleFunc("/signin", signinHandler).Methods("GET", "HEAD")
	r.HandleFunc("/signin", signinPostHandler).Methods("POST")
	r.HandleFunc("/signout", signoutHandler)
	r.HandleFunc("/mypage", mypageHandler)
	r.HandleFunc("/memo/{memo_id}", memoHandler).Methods("GET", "HEAD")
	r.HandleFunc("/memo", memoPostHandler).Methods("POST")
	r.HandleFunc("/recent/{page:[0-9]+}", recentHandler)
	r.PathPrefix("/").Handler(http.FileServer(http.Dir("./public/")))
	http.Handle("/", r)
	log.Fatal(http.ListenAndServe(listenAddr, withTrace(r)))
}

func loadConfig(filename string) *Config {
	log.Printf("loading config file: %s", filename)
	f, err := ioutil.ReadFile(filename)
	if err != nil {
		log.Fatal(err)
		os.Exit(1)
	}
	var config Config
	err = json.Unmarshal(f, &config)
	if err != nil {
		log.Fatal(err)
		os.Exit(1)
	}
	return &config
}

func prepareHandler(w http.ResponseWriter, r *http.Request) {
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		baseUrl, _ = url.Parse("http://" + h)
	} else {
		baseUrl, _ = url.Parse("http://" + r.Host)
	}
}

func loadSession(w http.ResponseWriter, r *http.Request) (session *sessions.Session, err error) {
	store := sessions.NewFilesystemStore("/tmp", []byte(sessionSecret))
	return store.Get(r, sessionName)
}

func getUser(ctx context.Context, w http.ResponseWriter, r *http.Request, dbConn *sqlx.DB, session *sessions.Session) *User {
	userId := session.Values["user_id"]
	if userId == nil {
		return nil
	}
	userName := session.Values["username"]
	if userName == nil {
		return nil
	}

	user := &User{
		Id:       userId.(int),
		Username: userName.(string),
	}
	if user != nil {
		w.Header().Add("Cache-Control", "private")
	}
	return user
}

func antiCSRF(w http.ResponseWriter, r *http.Request, session *sessions.Session) bool {
	if r.FormValue("sid") != session.Values["token"] {
		code := http.StatusBadRequest
		http.Error(w, http.StatusText(code), code)
		return true
	}
	return false
}

func serverError(w http.ResponseWriter, err error) {
	log.Printf("error: %s", err)
	code := http.StatusInternalServerError
	http.Error(w, http.StatusText(code), code)
}

func notFound(w http.ResponseWriter) {
	code := http.StatusNotFound
	http.Error(w, http.StatusText(code), code)
}

func topHandler(w http.ResponseWriter, r *http.Request) {
	session, err := loadSession(w, r)
	if err != nil {
		serverError(w, err)
		return
	}
	prepareHandler(w, r)
	dbConn := <-dbConnPool
	defer func() {
		dbConnPool <- dbConn
	}()
	ctx := context.Background()
	user := getUser(ctx, w, r, dbConn, session)

	var totalCount int
	rows, err := dbConn.Query("SELECT count(*) AS c FROM memos WHERE is_private=0")
	if err != nil {
		serverError(w, err)
		return
	}
	if rows.Next() {
		rows.Scan(&totalCount)
	}
	rows.Close()

	rows, err = dbConn.Query("SELECT id, user, is_private, title, created_at FROM memos WHERE is_private=0 ORDER BY created_at DESC, id DESC LIMIT ?", memosPerPage)
	if err != nil {
		serverError(w, err)
		return
	}

	memos := make(Memos, 0)
	var userIds []int
	for rows.Next() {
		memo := Memo{}
		rows.Scan(&memo.Id, &memo.User, &memo.IsPrivate, &memo.Title, &memo.CreatedAt)
		memos = append(memos, &memo)
		userIds = append(userIds, memo.User)
	}

	sql := `SELECT id,username FROM users WHERE id IN (?)`
	sql, params, err := sqlx.In(sql, userIds)
	if err != nil {
		log.Fatal(err)
	}
	var users []User
	if err := sqlx.Select(dbConn, &users, sql, params...); err != nil {
		log.Fatal(err)
	}

	for _, m := range memos {
		for _, u := range users {
			if u.Id == m.User {
				m.Username = u.Username
			}
		}
	}

	rows.Close()

	v := &View{
		Total:     totalCount,
		Page:      0,
		PageStart: 1,
		PageEnd:   memosPerPage,
		Memos:     &memos,
		User:      user,
		Session:   session,
	}
	if err = tmpl.ExecuteTemplate(w, "index", v); err != nil {
		serverError(w, err)
	}
}

func recentHandler(w http.ResponseWriter, r *http.Request) {
	session, err := loadSession(w, r)
	if err != nil {
		serverError(w, err)
		return
	}
	prepareHandler(w, r)
	dbConn := <-dbConnPool
	defer func() {
		dbConnPool <- dbConn
	}()
	ctx := context.Background()
	user := getUser(ctx, w, r, dbConn, session)
	vars := mux.Vars(r)
	page, _ := strconv.Atoi(vars["page"])

	rows, err := dbConn.Query("SELECT count(*) AS c FROM memos WHERE is_private=0")
	if err != nil {
		serverError(w, err)
		return
	}
	var totalCount int
	if rows.Next() {
		rows.Scan(&totalCount)
	}
	rows.Close()

	rows, err = dbConn.Query("SELECT * FROM memos WHERE is_private=0 ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?", memosPerPage, memosPerPage*page)
	if err != nil {
		serverError(w, err)
		return
	}
	memos := make(Memos, 0)
	var userIds []int
	for rows.Next() {
		memo := Memo{}
		rows.Scan(&memo.Id, &memo.User, &memo.Content, &memo.IsPrivate, &memo.CreatedAt, &memo.UpdatedAt)
		userIds = append(userIds, memo.User)
		memos = append(memos, &memo)
	}
	sql := `SELECT id,username FROM users WHERE id IN (?)`
	sql, params, err := sqlx.In(sql, userIds)
	if err != nil {
		log.Fatal(err)
	}
	var users []User
	if err := sqlx.Select(dbConn, &users, sql, params...); err != nil {
		log.Fatal(err)
	}

	for _, m := range memos {
		for _, u := range users {
			if u.Id == m.User {
				m.Username = u.Username
			}
		}
	}

	if len(memos) == 0 {
		notFound(w)
		return
	}

	v := &View{
		Total:     totalCount,
		Page:      page,
		PageStart: memosPerPage*page + 1,
		PageEnd:   memosPerPage * (page + 1),
		Memos:     &memos,
		User:      user,
		Session:   session,
	}
	if err = tmpl.ExecuteTemplate(w, "index", v); err != nil {
		serverError(w, err)
	}
}

func signinHandler(w http.ResponseWriter, r *http.Request) {
	session, err := loadSession(w, r)
	if err != nil {
		serverError(w, err)
		return
	}
	prepareHandler(w, r)
	dbConn := <-dbConnPool
	defer func() {
		dbConnPool <- dbConn
	}()
	ctx := context.Background()
	user := getUser(ctx, w, r, dbConn, session)

	v := &View{
		User:    user,
		Session: session,
	}
	if err := tmpl.ExecuteTemplate(w, "signin", v); err != nil {
		serverError(w, err)
		return
	}
}

func signinPostHandler(w http.ResponseWriter, r *http.Request) {
	session, err := loadSession(w, r)
	if err != nil {
		serverError(w, err)
		return
	}
	prepareHandler(w, r)
	dbConn := <-dbConnPool
	defer func() {
		dbConnPool <- dbConn
	}()

	username := r.FormValue("username")
	password := r.FormValue("password")
	user := &User{}
	rows, err := dbConn.Query("SELECT id, username, password, salt FROM users WHERE username=?", username)
	if err != nil {
		serverError(w, err)
		return
	}
	if rows.Next() {
		rows.Scan(&user.Id, &user.Username, &user.Password, &user.Salt)
	}
	rows.Close()
	if user.Id > 0 {
		h := sha256.New()
		h.Write([]byte(user.Salt + password))
		if user.Password == fmt.Sprintf("%x", h.Sum(nil)) {
			session.Values["user_id"] = user.Id
			session.Values["token"] = fmt.Sprintf("%x", securecookie.GenerateRandomKey(32))
			session.Values["username"] = user.Username
			if err := session.Save(r, w); err != nil {
				serverError(w, err)
				return
			}
			if _, err := dbConn.Exec("UPDATE users SET last_access=now() WHERE id=?", user.Id); err != nil {
				serverError(w, err)
				return
			} else {
				http.Redirect(w, r, "/mypage", http.StatusFound)
			}
			return
		}
	}
	v := &View{
		Session: session,
	}
	if err := tmpl.ExecuteTemplate(w, "signin", v); err != nil {
		serverError(w, err)
		return
	}
}

func signoutHandler(w http.ResponseWriter, r *http.Request) {
	session, err := loadSession(w, r)
	if err != nil {
		serverError(w, err)
		return
	}
	prepareHandler(w, r)
	if antiCSRF(w, r, session) {
		return
	}

	http.SetCookie(w, sessions.NewCookie(sessionName, "", &sessions.Options{MaxAge: -1}))
	http.Redirect(w, r, "/", http.StatusFound)
}

func mypageHandler(w http.ResponseWriter, r *http.Request) {
	session, err := loadSession(w, r)
	if err != nil {
		serverError(w, err)
		return
	}
	prepareHandler(w, r)
	dbConn := <-dbConnPool
	defer func() {
		dbConnPool <- dbConn
	}()
	ctx := context.Background()
	user := getUser(ctx, w, r, dbConn, session)
	if user == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	rows, err := dbConn.Query("SELECT id, is_private, title, created_at FROM memos WHERE user=? ORDER BY created_at DESC", user.Id)
	if err != nil {
		serverError(w, err)
		return
	}
	memos := make(Memos, 0)
	for rows.Next() {
		memo := Memo{}
		rows.Scan(&memo.Id, &memo.IsPrivate, &memo.Title, &memo.CreatedAt)
		memos = append(memos, &memo)
	}
	v := &View{
		Memos:   &memos,
		User:    user,
		Session: session,
	}
	if err = tmpl.ExecuteTemplate(w, "mypage", v); err != nil {
		serverError(w, err)
	}
}

func memoHandler(w http.ResponseWriter, r *http.Request) {
	session, err := loadSession(w, r)
	if err != nil {
		serverError(w, err)
		return
	}
	prepareHandler(w, r)
	vars := mux.Vars(r)
	memoId := vars["memo_id"]
	dbConn := <-dbConnPool
	defer func() {
		dbConnPool <- dbConn
	}()
	ctx := context.Background()
	user := getUser(ctx, w, r, dbConn, session)

	q := `
		SELECT
			m.id,
			m.user,
			m.content,
			m.is_private,
			u.username,
			m.created_at,
			m.updated_at
		FROM
			memos as m
			inner join users as u on u.id = m.user
		WHERE
			m.id = ?
	`
	rows, err := dbConn.Query(q, memoId)
	if err != nil {
		serverError(w, err)
		return
	}
	memo := &Memo{}
	if rows.Next() {
		rows.Scan(&memo.Id, &memo.User, &memo.Content, &memo.IsPrivate, &memo.Username, &memo.CreatedAt, &memo.UpdatedAt)
		rows.Close()
	} else {
		notFound(w)
		return
	}
	if memo.IsPrivate == 1 {
		if user == nil || user.Id != memo.User {
			notFound(w)
			return
		}
	}

	var cond string
	if user != nil && user.Id == memo.User {
		cond = ""
	} else {
		cond = "AND is_private=0"
	}
	rows, err = dbConn.Query("SELECT id, content, is_private, created_at, updated_at FROM memos WHERE user=? "+cond+" ORDER BY created_at", memo.User)
	if err != nil {
		serverError(w, err)
		return
	}
	memos := make(Memos, 0)
	for rows.Next() {
		m := Memo{}
		rows.Scan(&m.Id, &m.Content, &m.IsPrivate, &m.CreatedAt, &m.UpdatedAt)
		memos = append(memos, &m)
	}
	rows.Close()
	var older *Memo
	var newer *Memo
	for i, m := range memos {
		if m.Id == memo.Id {
			if i > 0 {
				older = memos[i-1]
			}
			if i < len(memos)-1 {
				newer = memos[i+1]
			}
		}
	}

	v := &View{
		User:    user,
		Memo:    memo,
		Older:   older,
		Newer:   newer,
		Session: session,
	}
	if err = tmpl.ExecuteTemplate(w, "memo", v); err != nil {
		serverError(w, err)
	}
}

func memoPostHandler(w http.ResponseWriter, r *http.Request) {
	session, err := loadSession(w, r)
	if err != nil {
		serverError(w, err)
		return
	}
	prepareHandler(w, r)
	if antiCSRF(w, r, session) {
		return
	}
	dbConn := <-dbConnPool
	defer func() {
		dbConnPool <- dbConn
	}()

	ctx := context.Background()
	user := getUser(ctx, w, r, dbConn, session)
	if user == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	var isPrivate int
	if r.FormValue("is_private") == "1" {
		isPrivate = 1
	} else {
		isPrivate = 0
	}
	result, err := dbConn.Exec(
		"INSERT INTO memos (user, content, is_private, created_at) VALUES (?, ?, ?, now())",
		user.Id, r.FormValue("content"), isPrivate,
	)
	if err != nil {
		serverError(w, err)
		return
	}
	newId, _ := result.LastInsertId()
	http.Redirect(w, r, fmt.Sprintf("/memo/%d", newId), http.StatusFound)
}
