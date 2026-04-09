package domain

import (
	"context"
	"errors"
	"time"
)

// ─── Greške validacije ────────────────────────────────────────────────────────

var (
	ErrInvalidCurrency    = errors.New("nevalidna valuta za kategoriju računa")
	ErrInvalidPodvrsta    = errors.New("nevalidna podvrsta za tip računa")
	ErrAccountNotFound    = errors.New("račun nije pronađen")
	ErrForbidden          = errors.New("pristup odbijen")
	ErrNazivVecPostoji    = errors.New("naziv računa već postoji")
	ErrNazivIsti          = errors.New("novi naziv je isti kao trenutni")
	ErrPendingNotFound       = errors.New("zahtev nije pronađen")
	ErrAlreadyApproved       = errors.New("zahtev je već obrađen")
	ErrWrongCode             = errors.New("pogrešan verifikacioni kod")
	ErrCodeExpired           = errors.New("verifikacioni kod je istekao")
	ErrTooManyAttempts       = errors.New("previše neuspešnih pokušaja, zahtev je otkazan")
	ErrPendingAlreadyExists  = errors.New("za ovaj račun već postoji aktivan zahtev za promenu limita — odobrite ili sačekajte da istekne")
)

// Currency je čisti domenski objekat — ne zna za GORM niti za gRPC.
type Currency struct {
	ID     int64
	Naziv  string
	Oznaka string
}

func (Currency) TableName() string {
	return "core_banking.valuta"
}

// CurrencyRepository definiše ugovor prema sloju podataka.
// Implementacija živi u repository paketu.
type CurrencyRepository interface {
	GetAll(ctx context.Context) ([]Currency, error)
	GetByID(ctx context.Context, id int64) (*Currency, error)
}

// CurrencyService definiše ugovor prema sloju poslovne logike.
// Implementacija živi u service paketu.
type CurrencyService interface {
	GetCurrencies(ctx context.Context) ([]Currency, error)
}

// Delatnost je čisti domenski objekat — ne zna za GORM niti za gRPC.
type Delatnost struct {
	ID     int64
	Sifra  string
	Naziv  string
	Grana  string
	Sektor string
}

// DelatnostRepository definiše ugovor prema sloju podataka.
type DelatnostRepository interface {
	GetAll(ctx context.Context) ([]Delatnost, error)
}

// DelatnostService definiše ugovor prema sloju poslovne logike.
type DelatnostService interface {
	GetDelatnosti(ctx context.Context) ([]Delatnost, error)
}

// ─── Account ──────────────────────────────────────────────────────────────────

// Firma sadrži podatke o poslovnom subjektu (popunjava se samo za POSLOVNI račun).
type Firma struct {
	Naziv       string
	MaticniBroj string
	PIB         string
	DelatnostID int64
	Adresa      string
}

// CreateAccountInput je ulazni domenski objekat za kreiranje računa.
type CreateAccountInput struct {
	ZaposleniID      int64
	VlasnikID        int64
	ValutaID         int64
	Firma            *Firma // nil za lične račune
	KategorijaRacuna string // "TEKUCI" | "DEVIZNI"
	VrstaRacuna      string // "LICNI" | "POSLOVNI"
	Podvrsta         string // npr. "STANDARDNI", "STEDNI" — relevantno za TEKUCI+LICNI
	NazivRacuna      string
	StanjeRacuna     float64
}

// AccountListItem je projekcija računa za listu klijentovih računa.
type AccountListItem struct {
	ID                  int64
	BrojRacuna          string
	NazivRacuna         string
	VrstaRacuna         string
	KategorijaRacuna    string
	ValutaOznaka        string
	StanjeRacuna        float64
	RezervovanaSredstva float64
	RaspolozivoStanje   float64
}

// EmployeeAccountListItem je projekcija računa za portal zaposlenih.
// Sadrži podatke iz bank-service baze i ime/prezime vlasnika iz user-service-a.
type EmployeeAccountListItem struct {
	ID               int64
	BrojRacuna       string
	VrstaRacuna      string // "LICNI" | "POSLOVNI"
	KategorijaRacuna string // "TEKUCI" | "DEVIZNI"
	VlasnikID        int64
	ImeVlasnika      string
	PrezimeVlasnika  string
}

// AccountDetail je detaljna projekcija jednog računa.
type AccountDetail struct {
	ID                  int64
	BrojRacuna          string
	NazivRacuna         string
	VrstaRacuna         string
	KategorijaRacuna    string
	ValutaOznaka        string
	StanjeRacuna        float64
	RezervovanaSredstva float64
	RaspolozivoStanje   float64
	DnevniLimit         float64
	MesecniLimit        float64
	NazivFirme          *string // nil za LICNI
}

// Transakcija je domenski objekat za jednu transakciju.
type Transakcija struct {
	ID               int64
	RacunID          int64
	TipTransakcije   string
	Iznos            float64
	Opis             string
	VremeIzvrsavanja time.Time
	Status           string
}

// GetAccountTransactionsInput parametri za dohvat transakcija.
type GetAccountTransactionsInput struct {
	RacunID   int64
	SortBy    string // "datum" | "tip"
	SortOrder string // "ASC" | "DESC"
}

// RenameAccountInput parametri za promenu naziva.
type RenameAccountInput struct {
	VlasnikID int64
	AccountID int64
	NoviNaziv string
}

// UpdateLimitInput parametri za kreiranje pending akcije za promenu limita.
type UpdateLimitInput struct {
	VlasnikID    int64
	AccountID    int64
	DnevniLimit  float64
	MesecniLimit float64
}

// PendingAction je domenski objekat za akciju koja čeka mobilnu verifikaciju.
type PendingAction struct {
	ID              int64
	VlasnikID       int64
	RacunID         int64
	ActionType      string
	Opis            string
	ParamsJSON      string
	Status          string
	BrojRacuna      string
	ValutaOznaka    string
	DnevniLimit     float64
	MesecniLimit    float64
	CreatedAt       time.Time
	// Polja za PLACANJE/PRENOS — popunjena iz payment_intent.
	NazivPrimaoca      string
	BrojRacunaPrimaoca string
	Iznos              float64
}

// VerifyLimitInput parametri za verifikaciju koda i primenu limita.
type VerifyLimitInput struct {
	VlasnikID  int64
	ActionID   int64
	Code       string
}

// AccountRepository definiše ugovor prema sloju podataka za operacije sa računima.
type AccountRepository interface {
	// CreateAccount izvršava transakciju (firma + racun INSERT).
	// Vraća surogat PK (racun.id) novokreiranog računa.
	CreateAccount(ctx context.Context, input CreateAccountInput, brojRacuna string) (int64, error)

	// GetAllAccounts vraća sve aktivne račune svih klijenata (za portal zaposlenih).
	// BrojRacunaFilter je opcioni parcijalni match (ILIKE); "" = bez filtera.
	// Ime/prezime se popunjava naknadno u handler sloju.
	GetAllAccounts(ctx context.Context, brojRacunaFilter string) ([]EmployeeAccountListItem, error)

	// GetClientAccounts vraća aktivne račune klijenta sortirane po raspoloživom stanju DESC.
	GetClientAccounts(ctx context.Context, vlasnikID int64) ([]AccountListItem, error)

	// GetAccountDetail vraća detalje jednog računa; greška ako ne pripada klijentu.
	GetAccountDetail(ctx context.Context, accountID, vlasnikID int64) (*AccountDetail, error)

	// GetAccountTransactions vraća transakcije za račun koji pripada klijentu.
	GetAccountTransactions(ctx context.Context, input GetAccountTransactionsInput, vlasnikID int64) ([]Transakcija, error)

	// RenameAccount menja naziv računa uz validaciju vlasništva i jedinstvenosti naziva.
	RenameAccount(ctx context.Context, input RenameAccountInput) error

	// UpdateAccountLimit kreira pending action za promenu limita.
	// Vraća ID kreirane pending akcije.
	UpdateAccountLimit(ctx context.Context, input UpdateLimitInput) (int64, error)

	// GetPendingActions vraća sve PENDING akcije vlasnika.
	GetPendingActions(ctx context.Context, vlasnikID int64) ([]PendingAction, error)

	// GetPendingAction vraća jednu pending akciju; greška ako ne pripada vlasniku.
	GetPendingAction(ctx context.Context, actionID, vlasnikID int64) (*PendingAction, error)

	// ApprovePendingAction generiše verifikacioni kod za akciju.
	// Vraća kod, vreme isteka (UTC) i trajanje u sekundama.
	ApprovePendingAction(ctx context.Context, actionID, vlasnikID int64) (code string, expiresAt time.Time, err error)

	// VerifyAndApplyLimit proverava kod i primenjuje promenu limita.
	VerifyAndApplyLimit(ctx context.Context, input VerifyLimitInput) error

	// FindAccountIDByNumber vraća interni ID aktivnog računa sa datim brojem računa.
	// Vraća 0 (bez greške) kada račun ne postoji.
	FindAccountIDByNumber(ctx context.Context, brojRacuna string) (int64, error)
}

// AccountService definiše ugovor prema sloju poslovne logike.
type AccountService interface {
	CreateAccount(ctx context.Context, input CreateAccountInput) (int64, error)
	GetAllAccounts(ctx context.Context, brojRacunaFilter string) ([]EmployeeAccountListItem, error)
	GetClientAccounts(ctx context.Context, vlasnikID int64) ([]AccountListItem, error)
	GetAccountDetail(ctx context.Context, accountID, vlasnikID int64) (*AccountDetail, error)
	GetAccountTransactions(ctx context.Context, input GetAccountTransactionsInput, vlasnikID int64) ([]Transakcija, error)
	RenameAccount(ctx context.Context, input RenameAccountInput) error
	UpdateAccountLimit(ctx context.Context, input UpdateLimitInput) (int64, error)
	GetPendingActions(ctx context.Context, vlasnikID int64) ([]PendingAction, error)
	GetPendingAction(ctx context.Context, actionID, vlasnikID int64) (*PendingAction, error)
	ApprovePendingAction(ctx context.Context, actionID, vlasnikID int64) (code string, expiresAt time.Time, err error)
	VerifyAndApplyLimit(ctx context.Context, input VerifyLimitInput) error

	// FindAccountIDByNumber vraća interni ID aktivnog računa sa datim brojem računa.
	// Vraća 0 (bez greške) kada račun ne postoji.
	FindAccountIDByNumber(ctx context.Context, brojRacuna string) (int64, error)
}
