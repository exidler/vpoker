package poker

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/nchern/vpoker/pkg/logger"
)

const (
	chipWidth = 70
)

var (
	// color: #3498DB; Blue
	// color: #F1C40F; Yellow
	PlayerColors = []Color{
		"#FF5733", // Red
		"#9B59B6", // Purple
		"#2ECC71", // Green
	}
)

// User represents a system user
type User struct {
	CreatedAt time.Time

	ID uuid.UUID

	Name string
}

// NewUser creates a new instance of a User
func NewUser(iD uuid.UUID, name string, createdAt time.Time) *User {
	return &User{
		CreatedAt: createdAt,
		ID:        iD,
		Name:      name,
	}
}

// Ranks represents all possible card ranks
var Ranks = []string{"2", "3", "4", "5", "6", "7", "8", "9", "10", "J", "Q", "K", "A"}

// Side is a card side
type Side string

const (
	Cover Side = "cover"
	Face  Side = "face"
)

// Suit is a card suit
type Suit string

const (
	BlankSuit      = ""
	Spades    Suit = "♠"
	Hearts    Suit = "♥"
	Diamonds  Suit = "♦"
	Clubs     Suit = "♣"
)

type Card struct {
	Suit Suit   `json:"suit"`
	Rank string `json:"rank"`
	Side Side   `json:"side"`
}

// Color is a card color
type Color string

const (
	Red   Color = "red"
	Blue  Color = "blue"
	Gray  Color = "gray"
	Green Color = "green"
	Black Color = "black"
)

var chipsSet = []Chip{
	{Color: Gray, Val: 1},
	{Color: Red, Val: 5},
	{Color: Blue, Val: 10},
	{Color: Green, Val: 25},
	{Color: Black, Val: 50},
}

// Chip represents a poker chip
type Chip struct {
	Color Color `json:"color"`
	Val   int   `json:"val"`
}

// PushType represents a push type
type PushType string

const (
	Refresh      PushType = "refresh"
	PlayerJoined PushType = "player_joined"
	UpdateItems  PushType = "update_items"
)

// Push represents a push event that happens in the game and
// carries objects to push to a client
type Push struct {
	Type PushType `json:"type"`

	Items []*TableItem `json:"items"`

	Players map[uuid.UUID]*Player `json:"players"`
}

// DeepCopy creates a deep copy of this push via serialisation
func (p *Push) DeepCopy() (*Push, error) {
	var dest *Push
	b, err := json.Marshal(&p)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &dest); err != nil {
		return nil, err
	}
	return dest, nil
}

func NewPushItems(items ...*TableItem) *Push {
	return &Push{Type: UpdateItems, Items: items}
}

func NewPushPlayerJoined(players map[uuid.UUID]*Player, items ...*TableItem) *Push {
	return &Push{
		Type: PlayerJoined,

		Items:   items,
		Players: players,
	}
}

func NewPushRefresh() *Push { return &Push{Type: Refresh} }

type PlayerList []*Player

func (pl PlayerList) NotifyAll(push *Push) {
	for _, p := range pl {
		// logger.Debug.Printf("recepient=%s send_push_begin", p.Name)
		p.Dispatch(push)
		// logger.Debug.Printf("recepient=%s send_push_finish", p.Name)
	}
}

// Player represents a player at the game table
type Player struct {
	*User

	// Color is an in game color of this player
	Color Color `json:"color"`

	// Skin represents a personalised style of this player
	Skin string `json:"skin"`

	// Index represents player index in slots
	Index int `json:"index"`

	updates chan *Push
}

func newPlayer(u *User, c Color) *Player {
	return &Player{
		Color: c,
		User:  u,
	}
}

// Dispatch sends an update to this player
func (p *Player) Dispatch(push *Push) *Player {
	defer func() {
		if r := recover(); r != nil {
			logger.Error.Printf("Player.Dispatch name=%s panic: %s", p.Name, r)
		}
	}()
	if p.updates != nil {
		p.updates <- push
	}
	return p
}

// Subscribe subscribes this player to async updates
func (p *Player) Subscribe(updates chan *Push) *Player {
	if p.updates != nil {
		defer func() {
			if r := recover(); r != nil {
				logger.Error.Printf("Player.Subscribe name=%s panic: %s", p.Name, r)
			}
		}()
		close(p.updates)
	}
	p.updates = updates
	return p
}

// Unsubscribe unsubscribes active update channel
func (p *Player) Unsubscribe() *Player {
	if p.updates != nil {
		close(p.updates)
		p.updates = nil
	}
	return p
}

// CardList is a list of cards
type CardList []*Card

// Class represents a type of the item on the table
type Class string

const (
	CardClass   Class = "card"
	ChipClass   Class = "chip"
	DealerClass Class = "dealer"
	PlayerClass Class = "player"
)

// TableItemList is a list of TableItems
type TableItemList []*TableItem

// Get retrieves an item from the list by its id
// XXX: O(n) implementation
// TODO: make lookups O(1)
func (l TableItemList) Get(id int) *TableItem {
	for _, it := range l {
		if it.ID == id {
			return it
		}
	}
	return nil
}

// TableItem represents a virtual object on the table
type TableItem struct {
	Card
	Chip

	Class Class `json:"class"`

	OwnerID string `json:"owner_id"`

	ID int `json:"id"`
	X  int `json:"x"`
	Y  int `json:"y"`
}

// NewTableItem creates a new table item
func NewTableItem(id int, x int, y int) *TableItem {
	return &TableItem{
		ID: id,
		X:  x,
		Y:  y,
	}
}

// AsCard makes this item as card
func (ti *TableItem) AsCard(c *Card) *TableItem {
	ti.Card = *c
	ti.Class = CardClass
	return ti
}

// AsChip makes this item as chip
func (ti *TableItem) AsChip(c *Chip) *TableItem {
	ti.Chip = *c
	ti.Class = ChipClass
	return ti
}

// AsDealer makes this item as dealer chip
func (ti *TableItem) AsDealer() *TableItem {
	ti.Class = DealerClass
	return ti
}

// AsPlayer makes this item as player object
func (ti *TableItem) AsPlayer(p *Player) *TableItem {
	ti.OwnerID = p.ID.String()
	ti.Class = PlayerClass
	return ti
}

// Is defines if this item belongs to a specified class
func (ti *TableItem) Is(cls Class) bool { return ti.Class == cls }

// IsOwnedBy checks if this item is owned by a specified user
func (ti *TableItem) IsOwnedBy(id uuid.UUID) bool {
	return ti.OwnerID == id.String()
}

// IsOwned checks if this item is owned by anyone
func (ti *TableItem) IsOwned() bool { return ti.OwnerID != "" }

// ApplyVisibilityRules evaluates visibility for fields of this item
// Currently it works for cards only preventing non owners to obtain
// information about card rank and suit.
func (ti *TableItem) ApplyVisibilityRules(curUser *User) {
	if !ti.Is(CardClass) {
		return // do nothing if this is not a card
	}
	if ti.IsOwnedBy(curUser.ID) {
		ti.Side = Face // owners always see their cards
	}
	isOwnedBySomeoneElse := ti.IsOwned() && !ti.IsOwnedBy(curUser.ID)
	if isOwnedBySomeoneElse {
		ti.Side = Cover // if a card is owned by someone, others always see their card cover
	}
	if ti.Side == Cover {
		ti.Rank = ""
		ti.Suit = BlankSuit
	}
}
