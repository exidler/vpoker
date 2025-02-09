package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/nchern/vpoker/pkg/httpx"
	"github.com/nchern/vpoker/pkg/logger"
	"github.com/nchern/vpoker/pkg/poker"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

const (
	maxPlayers      = 3
	cookieExpiresAt = 30 * 24 * time.Hour

	statePath = "/tmp/vpoker.json"
)

var (
	index      = template.Must(template.ParseFiles("web/index.html"))
	pokerTable = template.Must(template.ParseFiles("web/poker.html"))
	profile    = template.Must(template.ParseFiles("web/profile.html"))

	errChanClosed = errors.New("channel closed")
)

type m map[string]any

func logError(err error, tag string) {
	if err != nil {
		logger.Error.Printf("%s: %s", tag, err)
	}
}

type session struct {
	UserID    uuid.UUID `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
	Name      string    `json:"name"`

	user *poker.User `json:"-"`
}

func (s *session) toJSON() []byte {
	b, err := json.Marshal(s)
	dieIf(err)
	return b
}

func (s *session) toCookie() string {
	return base64.URLEncoding.EncodeToString(s.toJSON())
}

func (s *session) parseFromCookie(v string) error {
	if v == "" {
		return fmt.Errorf("empty cookie")
	}
	b, err := base64.URLEncoding.DecodeString(v)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return err
	}
	return nil
}

func randomString() string {
	number := rand.Intn(10000) + 1
	return strconv.Itoa(number)
}

func newSessionCookie(now time.Time, v string) *http.Cookie {
	return &http.Cookie{
		Path:    "/",
		Value:   v,
		Name:    "session",
		Expires: now.Add(cookieExpiresAt),
	}
}

func newEmptySession() *http.Cookie {
	return &http.Cookie{
		Path:   "/",
		MaxAge: 0, // Deletes the cookie immediately
		Name:   "session",
		Value:  "",
	}
}

func newLastName(now time.Time, v string) *http.Cookie {
	return &http.Cookie{
		Path:    "/",
		Value:   v,
		Name:    "last_name",
		Expires: now.Add(cookieExpiresAt),
	}
}

type Context struct {
	ctx   context.Context
	table *poker.Table
	user  *poker.User
}

func (c *Context) String() string {
	fields := []string{fmt.Sprintf("request_id=%s", httpx.RequestID(c.ctx))}
	if c.user != nil {
		fields = append(fields, "user_name="+c.user.Name)
	}
	if c.table != nil {
		fields = append(fields, "table_id="+c.table.ID.String())
	}
	return strings.Join(fields, " ")
}

type contextBuilder struct {
	err error

	ctx *Context
}

func newContextBuilder(ctx context.Context) *contextBuilder {
	return &contextBuilder{
		ctx: &Context{
			ctx: ctx,
		},
	}
}

func (b *contextBuilder) build() (*Context, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.ctx, nil
}

func (b *contextBuilder) withTable(s *server, r *http.Request, idParam string) *contextBuilder {
	if b.err != nil {
		return b
	}
	id := mux.Vars(r)[idParam]
	tableID, err := uuid.Parse(id)
	if err != nil {
		logger.Error.Println("bad uuid=" + id)
		b.err = httpx.NewError(http.StatusBadRequest, "bad id: "+id)
		return b
	}
	table, found := s.tables.Get(tableID)
	if !found {
		b.err = httpx.NewError(http.StatusNotFound, "table not found")
		return b
	}
	b.ctx.table = table
	return b
}

func (b *contextBuilder) withUser(s *server, r *http.Request) *contextBuilder {
	if b.err != nil {
		return b
	}
	sess, err := getUserFromSession(r, s.users)
	if err != nil {
		b.err = err
		return b
	}
	b.ctx.user = sess.user
	return b
}

const retPathKey = "ret_path"

func sanitizedRetpath(u *url.URL) string {
	s := u.Query().Get(retPathKey)
	if !strings.HasPrefix(s, "/") {
		logger.Info.Printf("bad ret_path: %s", s)
		return ""
	}
	if len(s) > 1024 {
		logger.Info.Printf("ret_path too long: %s", s)
		return ""
	}
	return s
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type ItemUpdatedResponse struct {
	Updated *poker.TableItem `json:"updated"`
}

type stateFile struct {
	path string
	lock sync.RWMutex
}

func NewStateFile(path string) *stateFile {
	return &stateFile{path: path}
}

func (s *stateFile) save(marshalers ...json.Marshaler) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	f, err := os.Create(statePath)
	defer func() { logError(f.Close(), "stateFile.save os.Create") }()
	if err != nil {
		return err
	}
	for _, v := range marshalers {
		b, err := v.MarshalJSON()
		if err != nil {
			return err
		}
		if _, err := f.Write(b); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(f); err != nil {
			return err
		}
	}
	return err
}

func (s *stateFile) load(unmarshalers ...json.Unmarshaler) error {
	s.lock.RLock()
	defer s.lock.RUnlock()
	if err := os.MkdirAll(path.Dir(statePath), 0700); err != nil {
		return err
	}
	f, err := os.Open(statePath)
	defer func() { logError(f.Close(), "stateFile.load os.Open") }()
	if err != nil {
		return err
	}
	r := bufio.NewReader(f)
	for _, v := range unmarshalers {
		l, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		if err := v.UnmarshalJSON([]byte(l)); err != nil {
			return err
		}
	}
	return nil
}

type server struct {
	endpoint string

	tables poker.TableMap
	users  poker.UserMap

	state *stateFile
}

func handlePush(ctx *Context, conn *websocket.Conn, update *poker.Push) error {
	if update == nil {
		// channel closed, teminating this update loop
		msg := "terminated by another connection"
		logger.Info.Printf("ws %s web socket connection %s", ctx, msg)
		if err := conn.WriteMessage(
			websocket.TextMessage, []byte(msg)); err != nil {
			logger.Error.Printf("%s conn.WriteMessage %s", ctx, err)
		}
		return errChanClosed
	}
	logger.Debug.Printf("ws %s push_begin: %s", ctx, update.Type)
	resp, err := update.DeepCopy()
	if err != nil {
		return err
	}
	for _, it := range resp.Items {
		it.ApplyVisibilityRules(ctx.user)
	}
	if err := conn.WriteJSON(resp); err != nil {
		return fmt.Errorf("conn.WriteJSON: %w", err)
	}
	logger.Debug.Printf("%s push_finished: %s", ctx, update.Type)
	return nil
}

func (s *server) pushTableUpdates(w http.ResponseWriter, r *http.Request) {
	// TODO: finalize channel properly. Now any error yields to deadlock.
	// IT IS NOT CLEAR HOW how to gracefully finalize channel on errors.
	// It means that for now there is a goroutine leak on disconnected web sockets
	// now it leads to race conditions when a new channel is created
	httpx.H(authenticated(s.users, func(r *http.Request) (*httpx.Response, error) {
		ctx, err := newContextBuilder(r.Context()).withUser(s, r).withTable(s, r, "id").build()
		if err != nil {
			return nil, err
		}
		var p *poker.Player
		updates := make(chan *poker.Push)
		if err := ctx.table.Update(func(t *poker.Table) error {
			p = t.Players[ctx.user.ID]
			if p == nil {
				return httpx.NewError(http.StatusForbidden, "you are not at the table")
			}
			p.Subscribe(updates)
			return nil
		}); err != nil {
			return nil, err
		}
		hdrs := http.Header{}
		hdrs.Set(httpx.RequestHeaderName, httpx.RequestID(ctx.ctx))
		conn, err := upgrader.Upgrade(w, r, hdrs) // after .Upgrade normal http responses are not posible
		if err != nil {
			return nil, fmt.Errorf("upgrader.Upgrade: %w", err)
		}
		defer conn.Close()
		logger.Debug.Printf("ws %s pushes_start", ctx)
		for {
			var err error
			select {
			case update := <-updates:
				if err = handlePush(ctx, conn, update); err != nil {
					if errors.Is(err, errChanClosed) {
						return nil, err // terminate the loop only if channel got closed
					}
					logger.Error.Printf("ws %s %s", ctx, err)
				}
			case <-time.After(15 * time.Second): // check state periodically
				// logger.Debug.Printf("%s websocket_ping", ctx)
				if err = conn.WriteMessage(websocket.PingMessage, []byte("ping")); err != nil {
					logger.Error.Printf("ws %s %s", ctx, err)
					// decide how to unsubscribe - race conditions
					// unable to write - close this connection
					// return nil, fmt.Errorf("websocket_ping write error: %w", err)
				}
				// no need to read - browser does not automatically send a response
			}
			if errors.Is(err, syscall.EPIPE) {
				close(updates)
				logger.Info.Printf("ws %s pushes_finish", ctx)
				return nil, err // terminate the loop
			}
		}
	}))(w, r)
}

func (s *server) shuffle(r *http.Request) (*httpx.Response, error) {
	ctx, err := newContextBuilder(r.Context()).withUser(s, r).withTable(s, r, "id").build()
	if err != nil {
		return nil, err
	}
	var notifyThem poker.PlayerList
	if err := ctx.table.Update(func(t *poker.Table) error {
		if t.Players[ctx.user.ID] == nil {
			return httpx.NewError(http.StatusForbidden, "you are not at the table")
		}
		ctx.table.Shuffle()
		notifyThem = t.OtherPlayers(ctx.user)
		return nil
	}); err != nil {
		return nil, err
	}
	// notify others
	// push updates: potentially long operation - check
	notifyThem.NotifyAll(poker.NewPushRefresh())
	return httpx.Redirect(fmt.Sprintf("/games/%s", ctx.table.ID)), nil
}

func (s *server) showCard(r *http.Request) (*httpx.Response, error) {
	ctx, err := newContextBuilder(r.Context()).withUser(s, r).withTable(s, r, "id").build()
	if err != nil {
		return nil, err
	}
	req := map[string]int{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	id, found := req["id"]
	if !found {
		return nil, httpx.NewError(http.StatusBadRequest, "id field is missing")
	}
	var updated poker.TableItem
	var notifyThem poker.PlayerList
	if err := ctx.table.Update(func(t *poker.Table) error {
		if t.Players[ctx.user.ID] == nil {
			return httpx.NewError(http.StatusForbidden, "you are not at the table")
		}
		item := t.Items.Get(id)
		if item == nil {
			return httpx.NewError(http.StatusNotFound, "item not found")
		}
		if err := item.Show(ctx.user); err != nil {
			return err
		}
		updated = *item
		notifyThem = t.OtherPlayers(ctx.user)
		return nil
	}); err != nil {
		return nil, err
	}
	// push updates: potentially long operation - check
	notifyThem.NotifyAll(poker.NewPushItems(&updated))
	return httpx.JSON(http.StatusOK, &ItemUpdatedResponse{Updated: &updated}), nil
}

func (s *server) takeCard(r *http.Request) (*httpx.Response, error) {
	ctx, err := newContextBuilder(r.Context()).withUser(s, r).withTable(s, r, "id").build()
	if err != nil {
		return nil, err
	}
	req := map[string]int{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	id, found := req["id"]
	if !found {
		return nil, httpx.NewError(http.StatusBadRequest, "id field is missing")
	}
	var updated poker.TableItem
	if err := ctx.table.Update(func(t *poker.Table) error {
		if t.Players[ctx.user.ID] == nil {
			return httpx.NewError(http.StatusForbidden, "you are not at the table")
		}
		item := t.Items.Get(id)
		if item == nil {
			return httpx.NewError(http.StatusNotFound, "item not found")
		}
		updated = *item.Take(ctx.user)
		return nil
	}); err != nil {
		return nil, err
	}
	updated.Side = poker.Face
	// push updates: potentially long operation - check
	ctx.table.ReadLock(func(t *poker.Table) error {
		t.OtherPlayers(ctx.user).NotifyAll(poker.NewPushItems(&updated))
		return nil
	})
	return httpx.JSON(http.StatusOK, ItemUpdatedResponse{Updated: &updated}), nil
}

func (s *server) profile(r *http.Request) (*httpx.Response, error) {
	sess, err := getUserFromSession(r, s.users)
	if err != nil {
		return nil, err
	}
	return httpx.RenderFile(http.StatusOK, "web/profile.html", m{
		"Retpath":  sanitizedRetpath(r.URL),
		"Username": sess.user.Name,
	})
}

func (s *server) updateProfile(r *http.Request) (*httpx.Response, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	sess, err := getUserFromSession(r, s.users)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(r.FormValue("user_name"))
	if name == "" {
		return httpx.String(http.StatusBadRequest, "empty name"), nil
	}
	if err := s.users.Update(sess.user.ID, func(u *poker.User) error {
		u.Name = name
		sess.user = u
		return nil
	}); err != nil {
		return nil, err
	}
	lastNameCookie := newLastName(time.Now(), name)
	if retPath := sanitizedRetpath(r.URL); retPath != "" {
		return httpx.Redirect(retPath).SetCookie(lastNameCookie), nil
	}
	return httpx.Render(
		http.StatusOK,
		profile,
		m{"Username": sess.user.Name},
		lastNameCookie)
}

func updateItem(ctx *Context, r *http.Request) (*poker.TableItem, error) {
	curUser, table := ctx.user, ctx.table
	if table.Players[curUser.ID] == nil {
		return nil, httpx.NewError(http.StatusForbidden, "you are not at the table")
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	logger.Debug.Printf("%s update: %s", ctx, string(b))
	var src poker.TableItem
	if err := json.Unmarshal(b, &src); err != nil {
		return nil, err
	}
	dest := table.Items.Get(src.ID)
	if dest == nil {
		return nil, httpx.NewError(http.StatusNotFound, "item not found")
	}
	if dest.Class != src.Class {
		return nil, httpx.NewError(http.StatusBadRequest, "attempt to update readonly field .Class")
	}
	dest.X = src.X
	dest.Y = src.Y
	if dest.Side != src.Side {
		if !dest.IsOwned() || dest.IsOwnedBy(curUser.ID) {
			// card can be turned if it's not taken or by the owner only
			dest.Side = src.Side
		}
	}
	return dest, nil
}

func (s *server) updateTable(r *http.Request) (*httpx.Response, error) {
	ctx, err := newContextBuilder(r.Context()).withUser(s, r).withTable(s, r, "id").build()
	if err != nil {
		return nil, err
	}
	curUser, table := ctx.user, ctx.table
	var notifyThem poker.PlayerList
	var updated poker.TableItem
	if err := table.Update(func(t *poker.Table) error {
		up, err := updateItem(ctx, r)
		if err != nil {
			return err
		}
		updated = *up
		// collect players to push updates to
		// push itself must happen outside table lock in order to avoid deadlocks
		notifyThem = table.OtherPlayers(curUser)
		return nil
	}); err != nil {
		return nil, err
	}
	logger.Debug.Printf("%s update dest=%+v", ctx, updated)
	// push updates: potentially long operation - check
	notifyThem.NotifyAll(poker.NewPushItems(&updated))
	return httpx.JSON(http.StatusOK, ItemUpdatedResponse{Updated: &updated}), nil
}

func (s *server) joinTable(r *http.Request) (*httpx.Response, error) {
	ctx, err := newContextBuilder(r.Context()).withUser(s, r).withTable(s, r, "id").build()
	if err != nil {
		return nil, err
	}
	var players map[uuid.UUID]*poker.Player
	var updated []*poker.TableItem
	var notifyThem poker.PlayerList
	if err := ctx.table.Update(func(t *poker.Table) error {
		hasJoined := t.Players[ctx.user.ID] != nil
		if hasJoined {
			return nil
		}
		logger.Debug.Printf("players_joind=%d", len(t.Players))
		if len(t.Players) >= maxPlayers {
			return httpx.NewError(http.StatusForbidden, "this table is full")
		}
		updated = t.Join(ctx.user)
		players = t.Players
		notifyThem = t.OtherPlayers(ctx.user)
		return nil
	}); err != nil {
		return nil, err
	}
	// push updates: potentially long operation - check
	logger.Debug.Printf("%d", len(updated))
	notifyThem.NotifyAll(poker.NewPushPlayerJoined(players, updated...))
	return httpx.Redirect(fmt.Sprintf("/games/%s", ctx.table.ID)), nil
}

func (s *server) renderTable(r *http.Request) (*httpx.Response, error) {
	ctx, err := newContextBuilder(r.Context()).withUser(s, r).withTable(s, r, "id").build()
	if err != nil {
		return nil, err
	}
	curUser, table := ctx.user, ctx.table
	players := []*poker.Player{}
	errRedirect := errors.New("redirect")
	if err := table.ReadLock(func(t *poker.Table) error {
		if t.Players[curUser.ID] == nil {
			return errRedirect
		}
		for _, v := range t.Players {
			players = append(players, v)
		}
		return nil
	}); err != nil {
		if err == errRedirect {
			return httpx.Redirect(fmt.Sprintf("/games/%s/join", table.ID)), nil
		}
		return nil, err
	}
	return httpx.RenderFile(http.StatusOK, "web/poker.html", m{
		"Players":  players,
		"TableID":  table.ID,
		"Username": curUser.Name,
	})
}

func getTableState(curUser *poker.User, table *poker.Table) (*poker.Table, error) {
	var tableCopy *poker.Table
	if err := table.ReadLock(func(t *poker.Table) error {
		if t.Players[curUser.ID] == nil {
			return httpx.NewError(http.StatusForbidden, "you are not at the table")
		}
		var err error
		// deep copy the current table - items must be modified
		// as their content differes for different users due to ownership
		tableCopy, err = t.DeepCopy()
		return err
	}); err != nil {
		return nil, err
	}
	for _, it := range tableCopy.Items {
		it.ApplyVisibilityRules(curUser)
	}
	return tableCopy, nil
}

func (s *server) tableState(r *http.Request) (*httpx.Response, error) {
	ctx, err := newContextBuilder(r.Context()).withUser(s, r).withTable(s, r, "id").build()
	if err != nil {
		return nil, err
	}
	tableCopy, err := getTableState(ctx.user, ctx.table)
	if err != nil {
		return nil, err
	}
	return httpx.JSON(http.StatusOK, tableCopy), nil
}

func (s *server) newTable(r *http.Request) (*httpx.Response, error) {
	sess, err := getUserFromSession(r, s.users)
	if err != nil {
		return nil, err
	}
	curUser := sess.user
	logger.Info.Printf("user_id=%s action=table_created", curUser.ID)

	table := poker.NewTable(uuid.New(), 50).StartGame()
	s.tables.Set(table.ID, table)
	table.Join(curUser)

	return httpx.Redirect(fmt.Sprintf("/games/%s", table.ID)), nil
}

func (s *server) newUser(r *http.Request) (*httpx.Response, error) {
	redirectTo := sanitizedRetpath(r.URL)
	if redirectTo == "" {
		redirectTo = "/"
	}
	old, err := getUserFromSession(r, s.users)
	if err != nil || old.user == nil {
		// Cookie not found or empty: create and set a new one
		name := "Anon" + randomString()
		if ln, err := r.Cookie("last_name"); err == nil {
			if ln.Value != "" {
				name = ln.Value
			}
		}
		now := time.Now()
		u := poker.NewUser(uuid.New(), name, now)
		s.users.Set(u.ID, u)
		sess := &session{UserID: u.ID, CreatedAt: now, Name: u.Name}
		cookie := newSessionCookie(now, sess.toCookie())
		ctx, err := newContextBuilder(r.Context()).build()
		if err != nil {
			return nil, err
		}
		logger.Info.Printf("%s user_name=%s user_registered", ctx, name)
		return httpx.Redirect(redirectTo).
			SetCookie(cookie).
			SetCookie(newLastName(now, name)), nil
	}
	return httpx.Redirect(redirectTo), nil
}

func (s *server) index(r *http.Request) (*httpx.Response, error) {
	logger.Debug.Printf("index.begin user_count=%d", s.users.Len())
	sess := &session{}
	cookie, err := r.Cookie("session")
	if err != nil || cookie.Value == "" {
		// no session cookie
		return httpx.Redirect("/users/new"), nil
	}
	logger.Debug.Printf("cookie_exists: session=%s", cookie.Value)
	if err := sess.parseFromCookie(cookie.Value); err != nil {
		logger.Info.Printf("bad cookie: %s", err)
		return httpx.Redirect("/users/new").
			SetCookie(newEmptySession()), nil
	}
	curUser, found := s.users.Get(sess.UserID)
	if !found {
		return httpx.Redirect("/users/new").
			SetCookie(newEmptySession()), nil
	}

	return httpx.RenderFile(http.StatusOK, "web/index.html", m{
		"Username": curUser.Name,
	})
}

func (s *server) loadState() error { return s.state.load(s.users, s.tables) }

func (s *server) saveState() error { return s.state.save(s.users, s.tables) }

func saveStateLoop(s *server) {
	const saveStateEvery = 10 * time.Second
	for range time.Tick(saveStateEvery) {
		if err := s.saveState(); err != nil {
			logger.Error.Printf("saveStateLoop: %s", err)
		}
	}
}

func getUserFromSession(r *http.Request, users poker.UserMap) (*session, error) {
	sess := &session{}
	cookie, err := r.Cookie("session")
	if err != nil {
		return sess, nil // no cookie - return new empty session
	}
	if err := sess.parseFromCookie(cookie.Value); err != nil {
		return nil, err
	}
	u, found := users.Get(sess.UserID)
	if found {
		sess.user = u
	}
	return sess, nil
}

func authenticated(users poker.UserMap, f httpx.RequestHandler) httpx.RequestHandler {
	return func(r *http.Request) (*httpx.Response, error) {
		sess, err := getUserFromSession(r, users)
		if err != nil {
			return nil, err
		}
		if sess.user == nil {
			return httpx.String(http.StatusUnauthorized), nil
		}
		return f(r)
	}
}

func handleSignalsLoop(srv *server) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	for s := range signals {
		logger.Info.Printf("received: %s; saving state...", s)
		if err := srv.saveState(); err != nil {
			logger.Error.Printf("server.saveState %s", err)
		}
		if s == syscall.SIGTERM || s == os.Interrupt {
			logger.Info.Printf("graceful shutdown: %s", s)
			break
		}
		os.Exit(1)
	}
	os.Exit(0)
}

// TODO_DEBUG: debug and test on mobile
// TODO: decide what to do with abandoned tables. Now they not only stay in memory but also
// keep websocket groutines/channels forever
func main() {
	s := &server{
		endpoint: ":8080",
		state:    NewStateFile(statePath),

		tables: poker.NewTableMapSyncronized(),
		users:  poker.NewUserMapSyncronized(),
	}
	if err := s.loadState(); err != nil {
		logger.Error.Printf("server.loadState %s", err)
	}
	auth := func(f httpx.RequestHandler) httpx.RequestHandler {
		return authenticated(s.users, f)
	}
	redirectIfNoAuth := func(url string, f httpx.RequestHandler) httpx.RequestHandler {
		return func(r *http.Request) (*httpx.Response, error) {
			resp, err := auth(f)(r)
			if err != nil {
				return nil, err
			}
			// logger.Debug.Println(resp.Code())
			if resp.Code() == http.StatusUnauthorized {
				return httpx.Redirect(fmt.Sprintf("%s?ret_path=%s", url, r.URL.Path)), nil
			}
			return resp, nil
		}
	}

	r := mux.NewRouter()

	r.HandleFunc("/", httpx.H(s.index)).Methods("GET")

	r.HandleFunc("/games/new", httpx.H(auth(s.newTable)))
	r.HandleFunc("/games/{id:[a-z0-9-]+}",
		httpx.H(redirectIfNoAuth("/users/new", s.renderTable))).Methods("GET")
	r.HandleFunc("/games/{id:[a-z0-9-]+}/state",
		httpx.H(auth(s.tableState))).Methods("GET")
	r.HandleFunc("/games/{id:[a-z0-9-]+}/join",
		httpx.H(auth(s.joinTable))).Methods("GET")
	r.HandleFunc("/games/{id:[a-z0-9-]+}/update",
		httpx.H(auth(s.updateTable))).Methods("POST")
	r.HandleFunc("/games/{id:[a-z0-9-]+}/show_card",
		httpx.H(auth(s.showCard))).Methods("POST")
	r.HandleFunc("/games/{id:[a-z0-9-]+}/take_card",
		httpx.H(auth(s.takeCard))).Methods("POST")
	r.HandleFunc("/games/{id:[a-z0-9-]+}/listen",
		s.pushTableUpdates).Methods("GET")
	r.HandleFunc("/games/{id:[a-z0-9-]+}/shuffle",
		httpx.H(auth(s.shuffle))).Methods("GET")

	r.HandleFunc("/users/new", httpx.H(s.newUser))
	r.HandleFunc("/users/profile",
		httpx.H(auth(s.profile))).
		Methods("GET")
	r.HandleFunc("/users/profile",
		httpx.H(auth(s.updateProfile))).
		Methods("POST")

	http.Handle("/", r)

	http.Handle("/static/",
		http.StripPrefix("/static/", http.FileServer(http.Dir("./web/static"))))

	go handleSignalsLoop(s)
	go saveStateLoop(s)

	logger.Info.Printf("Start listening on %s", s.endpoint)
	must(http.ListenAndServe(s.endpoint, nil))
}

func must(err error) {
	dieIf(err)
}

func dieIf(err error) {
	if err != nil {
		logger.Error.Println(err)
		os.Exit(1)
	}
}
