package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ericlagergren/decimal"
	"github.com/google/uuid"
)

var site = flag.String("site", "www.stocks.akhil.cc", "primary host name for site")

type html struct {
	Title   string
	Version int
}

type Error struct {
	Status int
	Msg    string
}

func (e Error) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Msg) }

// shift splits off the first component of u.Path, which will be cleaned of
// relative components before processing. head will never contain a slash and
// tail will always be a rooted path without trailing slash.
func shift(u *url.URL) string {
	p := path.Clean("/" + u.Path)
	i := strings.Index(p[1:], "/") + 1
	if i <= 0 {
		u.Path = "/"
		return p[1:]
	}
	u.Path = p[i:]
	return p[1:i]
}

type stock struct {
	Symbol    string
	Price     *money
	MarketCap string
}

type trades struct {
	frame   *template.Template
	stocks  map[string]stock
	v2mux   *sync.Mutex
	v2users map[uuid.UUID]map[string]int
	v3mux   *sync.Mutex
	v3users map[uuid.UUID]map[string]int
}

func (t *trades) list(w http.ResponseWriter, r *http.Request) *Error {
	values := make([]stock, 0, len(t.stocks))
	for _, v := range t.stocks {
		values = append(values, v)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Symbol < values[j].Symbol })
	if err := json.NewEncoder(w).Encode(values); err != nil {
		return internalServerError
	}
	return nil
}

func (t *trades) buy(u uuid.UUID, m *sync.Mutex, users map[uuid.UUID]map[string]int, w http.ResponseWriter, r *http.Request) *Error {
	symbol := shift(r.URL)
	quantity, err := strconv.Atoi(r.URL.Query().Get("quantity"))
	if err != nil {
		return badRequest
	}
	if _, ok := t.stocks[symbol]; !ok {
		return badRequest
	}
	m.Lock()
	defer m.Unlock()
	users[u][symbol] += quantity
	res := map[string]string{symbol: fmt.Sprintf("Bought %d shares", quantity)}
	if err := json.NewEncoder(w).Encode(res); err != nil {
		return internalServerError
	}
	return nil
}

func (t *trades) sell(u uuid.UUID, m *sync.Mutex, users map[uuid.UUID]map[string]int, w http.ResponseWriter, r *http.Request) *Error {
	symbol := shift(r.URL)
	quantity, err := strconv.Atoi(r.URL.Query().Get("quantity"))
	if err != nil {
		return badRequest
	}
	if _, ok := t.stocks[symbol]; !ok {
		return badRequest
	}
	m.Lock()
	defer m.Unlock()
	if users[u][symbol] < quantity {
		return badRequest
	}
	users[u][symbol] -= quantity
	res := map[string]string{symbol: fmt.Sprintf("Sold %d shares", quantity)}
	if err := json.NewEncoder(w).Encode(res); err != nil {
		return internalServerError
	}
	return nil
}

func (t *trades) auth(m *sync.Mutex, users map[uuid.UUID]map[string]int, w http.ResponseWriter, r *http.Request) *Error {
	m.Lock()
	defer m.Unlock()
	res := struct{ Token uuid.UUID }{uuid.New()}
	users[res.Token] = make(map[string]int)
	if err := json.NewEncoder(w).Encode(res); err != nil {
		return internalServerError
	}
	return nil
}

func (t *trades) v1(w http.ResponseWriter, r *http.Request) *Error {
	switch shift(r.URL) {
	case "":
		return t.template(w, html{"Stocks API Version 1: List", 1})
	case "list":
		return t.list(w, r)
	default:
		return notFound
	}
}

func (t *trades) checkAuth(m *sync.Mutex, users map[uuid.UUID]map[string]int, w http.ResponseWriter, r *http.Request) (u uuid.UUID, e *Error) {
	m.Lock()
	defer m.Unlock()
	param := r.URL.Query().Get("user")
	if param == "" {
		return
	}
	u, err := uuid.Parse(param)
	if err != nil {
		return u, unauthorized
	}
	if _, ok := users[u]; !ok {
		return u, unauthorized
	}
	return u, nil
}

func (t *trades) v2(w http.ResponseWriter, r *http.Request) *Error {
	u, err := t.checkAuth(t.v2mux, t.v2users, w, r)
	if err != nil {
		return err
	}
	s := shift(r.URL)
	if s == "" {
		return t.template(w, html{"Stocks API Version 2: List, Buy, and Sell", 2})
	}
	if u == (uuid.UUID{}) && s != "auth" {
		return unauthorized
	}
	switch s {
	case "list":
		return t.list(w, r)
	case "buy":
		return t.buy(u, t.v2mux, t.v2users, w, r)
	case "sell":
		return t.sell(u, t.v2mux, t.v2users, w, r)
	case "auth":
		return t.auth(t.v2mux, t.v2users, w, r)
	default:
		return notFound
	}
}

func (t *trades) v3(w http.ResponseWriter, r *http.Request) *Error {
	u, err := t.checkAuth(t.v3mux, t.v3users, w, r)
	if err != nil {
		return err
	}
	s := shift(r.URL)
	if s == "" {
		return t.template(w, html{"Stocks API Version 3: Flaky", 3})
	}
	if u == (uuid.UUID{}) && s != "auth" {
		return unauthorized
	}
	// 15% of the time, timeout for d seconds
	if d := rand.Intn(100); d < 15 {
		time.Sleep(time.Duration(d+1) * time.Second)
	}
	// 20% of the time, return an error
	if rand.Intn(100) < 20 {
		return internalServerError
	}
	switch s {
	case "list":
		return t.list(w, r)
	case "buy":
		return t.buy(u, t.v3mux, t.v3users, w, r)
	case "sell":
		return t.sell(u, t.v3mux, t.v3users, w, r)
	case "auth":
		return t.auth(t.v3mux, t.v3users, w, r)
	default:
		return notFound
	}
}

var notFound = &Error{http.StatusNotFound, "Not Found"}
var internalServerError = &Error{http.StatusInternalServerError, "Something Bad Happened"}
var unauthorized = &Error{http.StatusUnauthorized, "Unknown User"}
var badRequest = &Error{http.StatusBadRequest, "Invalid Request"}

func (t *trades) Handle(w http.ResponseWriter, r *http.Request) *Error {
	if r.Host != *site {
		return notFound
	}
	switch shift(r.URL) {
	case "v1":
		return t.v1(w, r)
	case "v2":
		return t.v2(w, r)
	case "v3":
		return t.v3(w, r)
	case "favicon.ico":
		w.Header().Set("Content-Type", "image/x-icon")
		return nil
	default:
		return t.template(w, html{"Stocks API", 0})
	}
}

func (t *trades) template(w http.ResponseWriter, h html) *Error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.frame.Execute(w, h); err != nil {
		w.Header().Set("Content-Type", "application/json")
		return internalServerError
	}
	return nil
}

func (t *trades) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := t.Handle(w, r); err != nil {
		w.WriteHeader(err.Status)
		res := map[string]string{"Error": err.Msg}
		json.NewEncoder(w).Encode(res)
	}
}

type money struct {
	*decimal.Big
}

func (m money) String() string {
	return "$" + m.Big.String()
}

func (m money) Format(state fmt.State, c rune) {
	state.Write([]byte{'$'})
	m.Big.Format(state, c)
}

func (m money) MarshalText() (b []byte, err error) {
	if b, err = m.Big.MarshalText(); err != nil {
		return nil, err
	}
	return append([]byte{'$'}, b...), nil
}

func (m *money) SetString(s string) bool {
	scale := map[byte]int{'M': -6, 'B': -9, 'T': -12}
	if len(s) > 0 && s[0] == '$' {
		s = s[1:]
	}
	var factor int
	if len(s) > 0 {
		if factor = scale[s[len(s)-1]]; factor != 0 {
			s = s[:len(s)-1]
		}
	}
	if _, ok := m.Big.SetString(s); !ok {
		return false
	}
	m.Big.Mul(m.Big, decimal.New(1, factor))
	return true
}

func mon(s string) *money {
	mon := &money{new(decimal.Big)}
	if ok := mon.SetString(s); !ok {
		return nil
	}
	return mon
}

func symbols(filename string) map[string]stock {
	f, err := os.Open(filename)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	stocks := make(map[string]stock)
	i := 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatal(err)
		}
		if i != 0 {
			stocks[rec[0]] = stock{
				Symbol:    rec[0],
				Price:     mon(rec[1]),
				MarketCap: rec[2],
			}
		}
		i++
	}
	return stocks
}

func main() {
	flag.Parse()
	log.Fatal(http.ListenAndServe(":8080", &trades{
		frame:   template.Must(template.ParseFiles("frame.tmpl")),
		stocks:  symbols("nasdaq.csv"),
		v2mux:   new(sync.Mutex),
		v2users: make(map[uuid.UUID]map[string]int),
		v3mux:   new(sync.Mutex),
		v3users: make(map[uuid.UUID]map[string]int),
	}))
}
