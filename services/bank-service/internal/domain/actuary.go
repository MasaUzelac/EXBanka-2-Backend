package domain

import (
	"context"
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

// ─── Greške aktuara ───────────────────────────────────────────────────────────

var (
	ErrActuaryNotFound      = errors.New("aktuar nije pronađen")
	ErrActuaryAlreadyExists = errors.New("zaposleni je već registrovan kao aktuar")
	ErrNotActuary           = errors.New("korisnik nije registrovan kao aktuar")
	ErrNotSupervisor        = errors.New("pristup odbijen: zahteva ulogu supervizora")
	ErrActuaryLimitExceeded = errors.New("nalog bi premašio dnevni limit agenta")
)

// ─── Tipovi ───────────────────────────────────────────────────────────────────

// ActuaryType razlikuje supervizore (bez limita) od agenata (dnevni limit).
type ActuaryType string

const (
	ActuaryTypeSupervisor ActuaryType = "SUPERVISOR"
	ActuaryTypeAgent      ActuaryType = "AGENT"
)

// ─── Domenska entiteta ────────────────────────────────────────────────────────

// Actuary je osnovna domenska entiteta za zaposlene koji trguju na berzi.
// Supervizori uvek imaju Limit=Zero, UsedLimit=Zero i NeedApproval=false.
type Actuary struct {
	ID           int64
	EmployeeID   int64
	ActuaryType  ActuaryType
	Limit        decimal.Decimal // dnevni limit troškova u RSD; Zero za supervizore
	UsedLimit    decimal.Decimal // potrošeno danas; resetuje se u 23:59 ili ručno
	NeedApproval bool            // supervizori uvek imaju false
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ─── Input DTO-ovi ────────────────────────────────────────────────────────────

// CreateActuaryInput ulazni parametri za kreiranje novog zapisa aktuara.
type CreateActuaryInput struct {
	EmployeeID   int64
	ActuaryType  ActuaryType
	Limit        decimal.Decimal
	UsedLimit    decimal.Decimal
	NeedApproval bool
}

// UpdateActuaryInput parametri za atomsku zamenu svih promenljivih polja.
type UpdateActuaryInput struct {
	ID           int64
	ActuaryType  ActuaryType
	Limit        decimal.Decimal
	UsedLimit    decimal.Decimal
	NeedApproval bool
}

// ─── Repository interfejs ─────────────────────────────────────────────────────

// ActuaryRepository definiše ugovor prema sloju podataka za Aktuar modul.
type ActuaryRepository interface {
	// Create kreira novi zapis aktuara i vraća persitovani objekat.
	Create(ctx context.Context, input CreateActuaryInput) (*Actuary, error)

	// GetByID vraća aktuara po surogat PK-u.
	// Vraća ErrActuaryNotFound ako ne postoji.
	GetByID(ctx context.Context, id int64) (*Actuary, error)

	// GetByEmployeeID vraća aktuara po employee_id (cross-service referenca).
	// Vraća ErrActuaryNotFound ako zaposleni nije registrovan kao aktuar.
	GetByEmployeeID(ctx context.Context, employeeID int64) (*Actuary, error)

	// List vraća aktuare filtrirane po tipu; "" vraća sve tipove.
	List(ctx context.Context, actuaryType string) ([]Actuary, error)

	// Update zamenjuje sva promenljiva polja i vraća ažurirani objekat.
	// Vraća ErrActuaryNotFound ako ne postoji.
	Update(ctx context.Context, input UpdateActuaryInput) (*Actuary, error)

	// Delete briše zapis aktuara po PK-u (idempotentno — ne vraća grešku ako ne postoji).
	Delete(ctx context.Context, id int64) error

	// DeleteByEmployeeID briše zapis aktuara po employee_id (idempotentno).
	DeleteByEmployeeID(ctx context.Context, employeeID int64) error

	// ResetAllUsedLimits atomski resetuje used_limit na '0.00' za sve agente (actuary_type = 'AGENT').
	ResetAllUsedLimits(ctx context.Context) error

	// IncrementUsedLimitIfWithin atomski povećava used_limit za agenta za dati iznos,
	// ALI SAMO ako (used_limit + amount) <= limit.
	// Vraća ažurirani Actuary i nil ako je operacija uspela.
	// Vraća (nil, ErrActuaryLimitExceeded) ako bi iznos premašio dnevni limit.
	// Ovo eliminuje TOCTOU race condition koji bi nastao pri odvojenom čitanju i pisanju.
	IncrementUsedLimitIfWithin(ctx context.Context, employeeID int64, amount decimal.Decimal) (*Actuary, error)
}

// ─── Service interfejs ────────────────────────────────────────────────────────

// ActuaryService definiše ugovor poslovne logike za Aktuar modul.
type ActuaryService interface {
	// Opšte operacije
	GetActuaryByID(ctx context.Context, id int64) (*Actuary, error)
	GetActuaryByEmployeeID(ctx context.Context, employeeID int64) (*Actuary, error)

	// Operacije supervizorskog portala
	ListAgents(ctx context.Context) ([]Actuary, error)
	SetAgentLimit(ctx context.Context, employeeID int64, limit decimal.Decimal) (*Actuary, error)
	ResetAgentUsedLimit(ctx context.Context, employeeID int64) (*Actuary, error)
	SetAgentNeedApproval(ctx context.Context, employeeID int64, needApproval bool) (*Actuary, error)

	// Interne operacije (poziva user-service pri promeni permisija)
	// CreateActuaryForEmployee kreira actuary_info zapis kad zaposleni dobije SUPERVISOR ili AGENT.
	CreateActuaryForEmployee(ctx context.Context, employeeID int64, actuaryType ActuaryType) (*Actuary, error)
	// DeleteActuaryForEmployee briše actuary_info zapis kad zaposleni izgubi SUPERVISOR ili AGENT.
	DeleteActuaryForEmployee(ctx context.Context, employeeID int64) error
	// ResetAllAgentsUsedLimit atomski resetuje used_limit na 0 za sve agente (poziva se u 23:59).
	ResetAllAgentsUsedLimit(ctx context.Context) error
}
