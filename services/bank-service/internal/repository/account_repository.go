package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"banka-backend/services/bank-service/internal/domain"

	"gorm.io/gorm"
)

// limitParams je struktura za JSON serijalizaciju params_json polja pending_action.
// -1 je sentinel vrednost koja znači "ne menjaj ovaj limit".
type limitParams struct {
	DnevniLimit  float64 `json:"dnevni_limit"`
	MesecniLimit float64 `json:"mesecni_limit"`
}

// currencyModel je GORM projekcija tabele currencies.
type currencyModel struct {
	ID     int64  `gorm:"column:id;primaryKey"`
	Naziv  string `gorm:"column:naziv"`
	Oznaka string `gorm:"column:oznaka"`
	Status bool   `gorm:"column:status"`
}

func (currencyModel) TableName() string {
	return "core_banking.valuta"
}

func (m currencyModel) toDomain() domain.Currency {
	return domain.Currency{
		ID:     m.ID,
		Naziv:  m.Naziv,
		Oznaka: m.Oznaka,
	}
}

type currencyRepository struct {
	db *gorm.DB
}

func NewCurrencyRepository(db *gorm.DB) domain.CurrencyRepository {
	return &currencyRepository{db: db}
}

func (r *currencyRepository) GetByID(ctx context.Context, id int64) (*domain.Currency, error) {
	var m currencyModel
	if err := r.db.WithContext(ctx).First(&m, id).Error; err != nil {
		return nil, err
	}
	c := m.toDomain()
	return &c, nil
}

func (r *currencyRepository) GetAll(ctx context.Context) ([]domain.Currency, error) {
	var models []currencyModel
	if err := r.db.WithContext(ctx).Where("status = ?", true).Find(&models).Error; err != nil {
		return nil, err
	}
	currencies := make([]domain.Currency, 0, len(models))
	for _, m := range models {
		currencies = append(currencies, m.toDomain())
	}
	return currencies, nil
}

// ── Delatnost ─────────────────────────────────────────────────────────────────

type delatnostModel struct {
	ID     int64  `gorm:"column:id;primaryKey"`
	Sifra  string `gorm:"column:sifra"`
	Naziv  string `gorm:"column:naziv"`
	Grana  string `gorm:"column:grana"`
	Sektor string `gorm:"column:sektor"`
}

func (delatnostModel) TableName() string {
	return "core_banking.delatnost"
}

func (m delatnostModel) toDomain() domain.Delatnost {
	return domain.Delatnost{
		ID:     m.ID,
		Sifra:  m.Sifra,
		Naziv:  m.Naziv,
		Grana:  m.Grana,
		Sektor: m.Sektor,
	}
}

type delatnostRepository struct {
	db *gorm.DB
}

func NewDelatnostRepository(db *gorm.DB) domain.DelatnostRepository {
	return &delatnostRepository{db: db}
}

func (r *delatnostRepository) GetAll(ctx context.Context) ([]domain.Delatnost, error) {
	var models []delatnostModel
	if err := r.db.WithContext(ctx).Find(&models).Error; err != nil {
		return nil, err
	}
	delatnosti := make([]domain.Delatnost, 0, len(models))
	for _, m := range models {
		delatnosti = append(delatnosti, m.toDomain())
	}
	return delatnosti, nil
}

// ── Account ───────────────────────────────────────────────────────────────────

type firmaModel struct {
	ID           int64  `gorm:"column:id;primaryKey"`
	NazivFirme   string `gorm:"column:naziv_firme"`
	MaticniBroj  string `gorm:"column:maticni_broj"`
	PoresikBroj  string `gorm:"column:poreski_broj"`
	IDDelatnosti int64  `gorm:"column:id_delatnosti"`
	Adresa       string `gorm:"column:adresa"`
	VlasnikID    int64  `gorm:"column:vlasnik_id"`
}

func (firmaModel) TableName() string { return "core_banking.firma" }

type racunModel struct {
	ID                  int64     `gorm:"column:id;primaryKey;autoIncrement"`
	BrojRacuna          string    `gorm:"column:broj_racuna;uniqueIndex"`
	IDZaposlenog        int64     `gorm:"column:id_zaposlenog"`
	IDVlasnika          int64     `gorm:"column:id_vlasnika"`
	IDFirme             *int64    `gorm:"column:id_firme"`
	IDValute            int64     `gorm:"column:id_valute"`
	KategorijaRacuna    string    `gorm:"column:kategorija_racuna"`
	VrstaRacuna         string    `gorm:"column:vrsta_racuna"`
	Podvrsta            *string   `gorm:"column:podvrsta"`
	NazivRacuna         string    `gorm:"column:naziv_racuna"`
	StanjeRacuna        float64   `gorm:"column:stanje_racuna"`
	RezervovanaSredstva float64   `gorm:"column:rezervisana_sredstva"`
	DatumKreiranja      time.Time `gorm:"column:datum_kreiranja"`
	DatumIsteka         time.Time `gorm:"column:datum_isteka"`
	Status              string    `gorm:"column:status"`
	DnevniLimit         float64   `gorm:"column:dnevni_limit"`
	MesecniLimit        float64   `gorm:"column:mesecni_limit"`
}

func (racunModel) TableName() string { return "core_banking.racun" }

type transakcijaModel struct {
	ID               int64     `gorm:"column:id;primaryKey;autoIncrement"`
	RacunID          int64     `gorm:"column:racun_id"`
	TipTransakcije   string    `gorm:"column:tip_transakcije"`
	Iznos            float64   `gorm:"column:iznos"`
	Opis             string    `gorm:"column:opis"`
	VremeIzvrsavanja time.Time `gorm:"column:vreme_izvrsavanja"`
	Status           string    `gorm:"column:status"`
}

func (transakcijaModel) TableName() string { return "core_banking.transakcija" }

type accountRepository struct {
	db *gorm.DB
}

func NewAccountRepository(db *gorm.DB) domain.AccountRepository {
	return &accountRepository{db: db}
}

// CreateAccount izvršava SQL transakciju: INSERT firma (opcionalno) → INSERT racun.
func (r *accountRepository) CreateAccount(ctx context.Context, input domain.CreateAccountInput, brojRacuna string) (int64, error) {
	var idFirme int64
	var racunID int64

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if input.VrstaRacuna == "POSLOVNI" && input.Firma != nil {
			f := &firmaModel{
				NazivFirme:   input.Firma.Naziv,
				MaticniBroj:  input.Firma.MaticniBroj,
				PoresikBroj:  input.Firma.PIB,
				IDDelatnosti: input.Firma.DelatnostID,
				Adresa:       input.Firma.Adresa,
				VlasnikID:    input.VlasnikID,
			}
			if err := tx.Create(f).Error; err != nil {
				return fmt.Errorf("insert firma: %w", err)
			}
			idFirme = f.ID
		}

		now := time.Now().UTC()

		var idFirmePtr *int64
		if idFirme != 0 {
			idFirmePtr = &idFirme
		}

		var podvrstaPtr *string
		if input.Podvrsta != "" {
			p := input.Podvrsta
			podvrstaPtr = &p
		}

		racun := &racunModel{
			BrojRacuna:       brojRacuna,
			IDZaposlenog:     input.ZaposleniID,
			IDVlasnika:       input.VlasnikID,
			IDFirme:          idFirmePtr,
			IDValute:         input.ValutaID,
			KategorijaRacuna: input.KategorijaRacuna,
			VrstaRacuna:      input.VrstaRacuna,
			Podvrsta:         podvrstaPtr,
			NazivRacuna:      input.NazivRacuna,
			StanjeRacuna:     input.StanjeRacuna,
			DatumKreiranja:   now,
			DatumIsteka:      now.AddDate(5, 0, 0),
			Status:           "AKTIVAN",
		}
		if err := tx.Create(racun).Error; err != nil {
			return fmt.Errorf("insert racun: %w", err)
		}
		racunID = racun.ID
		return nil
	})

	if err != nil {
		return 0, err
	}
	return racunID, nil
}

// accountListRow koristi se za JOIN rezultate u listama.
type accountListRow struct {
	ID                  int64   `gorm:"column:id"`
	BrojRacuna          string  `gorm:"column:broj_racuna"`
	NazivRacuna         string  `gorm:"column:naziv_racuna"`
	VrstaRacuna         string  `gorm:"column:vrsta_racuna"`
	KategorijaRacuna    string  `gorm:"column:kategorija_racuna"`
	ValutaOznaka        string  `gorm:"column:valuta_oznaka"`
	StanjeRacuna        float64 `gorm:"column:stanje_racuna"`
	RezervovanaSredstva float64 `gorm:"column:rezervisana_sredstva"`
}

// allAccountsRow je projekcija za listu svih računa (portal zaposlenih).
type allAccountsRow struct {
	ID               int64  `gorm:"column:id"`
	BrojRacuna       string `gorm:"column:broj_racuna"`
	VrstaRacuna      string `gorm:"column:vrsta_racuna"`
	KategorijaRacuna string `gorm:"column:kategorija_racuna"`
	IDVlasnika       int64  `gorm:"column:id_vlasnika"`
}

// GetAllAccounts vraća sve aktivne račune — bez filtera po vlasniku.
// brojRacunaFilter je opcioni parcijalni match (ILIKE); "" = bez filtera.
// Ime i prezime vlasnika se ne dohvataju ovde; popunjava ih handler sloj
// sinhronim pozivom ka user-service-u.
func (r *accountRepository) GetAllAccounts(ctx context.Context, brojRacunaFilter string) ([]domain.EmployeeAccountListItem, error) {
	var rows []allAccountsRow

	query := `
		SELECT
			ra.id,
			ra.broj_racuna,
			ra.vrsta_racuna,
			ra.kategorija_racuna,
			ra.id_vlasnika
		FROM core_banking.racun ra
		WHERE ra.status = 'AKTIVAN'
	`

	var err error
	if brojRacunaFilter != "" {
		query += " AND ra.broj_racuna ILIKE ?"
		err = r.db.WithContext(ctx).Raw(query, "%"+brojRacunaFilter+"%").Scan(&rows).Error
	} else {
		err = r.db.WithContext(ctx).Raw(query).Scan(&rows).Error
	}

	if err != nil {
		return nil, err
	}

	items := make([]domain.EmployeeAccountListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, domain.EmployeeAccountListItem{
			ID:               row.ID,
			BrojRacuna:       row.BrojRacuna,
			VrstaRacuna:      row.VrstaRacuna,
			KategorijaRacuna: row.KategorijaRacuna,
			VlasnikID:        row.IDVlasnika,
		})
	}
	return items, nil
}

// FindAccountIDByNumber vraća interni ID aktivnog računa sa tačno zadatim brojem.
// Vraća 0 (bez greške) ako račun ne postoji ili nije aktivan.
func (r *accountRepository) FindAccountIDByNumber(ctx context.Context, brojRacuna string) (int64, error) {
	var id int64
	err := r.db.WithContext(ctx).Raw(
		`SELECT id FROM core_banking.racun WHERE broj_racuna = ? AND status = 'AKTIVAN' LIMIT 1`,
		brojRacuna,
	).Scan(&id).Error
	if err != nil {
		return 0, fmt.Errorf("dohvat ID računa %s: %w", brojRacuna, err)
	}
	return id, nil
}

// GetClientAccounts vraća aktivne račune klijenta sortirane po raspoloživom stanju DESC.
func (r *accountRepository) GetClientAccounts(ctx context.Context, vlasnikID int64) ([]domain.AccountListItem, error) {
	var rows []accountListRow

	err := r.db.WithContext(ctx).Raw(`
		SELECT
			ra.id,
			ra.broj_racuna,
			ra.naziv_racuna,
			ra.vrsta_racuna,
			ra.kategorija_racuna,
			v.oznaka AS valuta_oznaka,
			ra.stanje_racuna,
			ra.rezervisana_sredstva
		FROM core_banking.racun ra
		JOIN core_banking.valuta v ON v.id = ra.id_valute
		WHERE ra.id_vlasnika = ?
		  AND ra.status = 'AKTIVAN'
		ORDER BY (ra.stanje_racuna - ra.rezervisana_sredstva) DESC
	`, vlasnikID).Scan(&rows).Error

	if err != nil {
		return nil, err
	}

	items := make([]domain.AccountListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, domain.AccountListItem{
			ID:                  row.ID,
			BrojRacuna:          row.BrojRacuna,
			NazivRacuna:         row.NazivRacuna,
			VrstaRacuna:         row.VrstaRacuna,
			KategorijaRacuna:    row.KategorijaRacuna,
			ValutaOznaka:        row.ValutaOznaka,
			StanjeRacuna:        row.StanjeRacuna,
			RezervovanaSredstva: row.RezervovanaSredstva,
			RaspolozivoStanje:   row.StanjeRacuna - row.RezervovanaSredstva,
		})
	}
	return items, nil
}

// accountDetailRow koristi se za JOIN rezultate u detail upitu.
type accountDetailRow struct {
	ID                  int64   `gorm:"column:id"`
	BrojRacuna          string  `gorm:"column:broj_racuna"`
	NazivRacuna         string  `gorm:"column:naziv_racuna"`
	VrstaRacuna         string  `gorm:"column:vrsta_racuna"`
	KategorijaRacuna    string  `gorm:"column:kategorija_racuna"`
	ValutaOznaka        string  `gorm:"column:valuta_oznaka"`
	StanjeRacuna        float64 `gorm:"column:stanje_racuna"`
	RezervovanaSredstva float64 `gorm:"column:rezervisana_sredstva"`
	DnevniLimit         float64 `gorm:"column:dnevni_limit"`
	MesecniLimit        float64 `gorm:"column:mesecni_limit"`
	NazivFirme          *string `gorm:"column:naziv_firme"`
}

// GetAccountDetail vraća detalje jednog računa; greška ako ne pripada klijentu.
func (r *accountRepository) GetAccountDetail(ctx context.Context, accountID, vlasnikID int64) (*domain.AccountDetail, error) {
	var row accountDetailRow

	err := r.db.WithContext(ctx).Raw(`
		SELECT
			ra.id,
			ra.broj_racuna,
			ra.naziv_racuna,
			ra.vrsta_racuna,
			ra.kategorija_racuna,
			v.oznaka AS valuta_oznaka,
			ra.stanje_racuna,
			ra.rezervisana_sredstva,
			COALESCE(ra.dnevni_limit, 0)  AS dnevni_limit,
			COALESCE(ra.mesecni_limit, 0) AS mesecni_limit,
			f.naziv_firme
		FROM core_banking.racun ra
		JOIN core_banking.valuta v ON v.id = ra.id_valute
		LEFT JOIN core_banking.firma f ON f.id = ra.id_firme
		WHERE ra.id = ?
		  AND ra.id_vlasnika = ?
		  AND ra.status = 'AKTIVAN'
		LIMIT 1
	`, accountID, vlasnikID).Scan(&row).Error

	if err != nil {
		return nil, err
	}
	if row.ID == 0 {
		return nil, domain.ErrAccountNotFound
	}

	return &domain.AccountDetail{
		ID:                  row.ID,
		BrojRacuna:          row.BrojRacuna,
		NazivRacuna:         row.NazivRacuna,
		VrstaRacuna:         row.VrstaRacuna,
		KategorijaRacuna:    row.KategorijaRacuna,
		ValutaOznaka:        row.ValutaOznaka,
		StanjeRacuna:        row.StanjeRacuna,
		RezervovanaSredstva: row.RezervovanaSredstva,
		RaspolozivoStanje:   row.StanjeRacuna - row.RezervovanaSredstva,
		DnevniLimit:         row.DnevniLimit,
		MesecniLimit:        row.MesecniLimit,
		NazivFirme:          row.NazivFirme,
	}, nil
}

// GetAccountTransactions vraća transakcije za račun koji pripada klijentu.
func (r *accountRepository) GetAccountTransactions(ctx context.Context, input domain.GetAccountTransactionsInput, vlasnikID int64) ([]domain.Transakcija, error) {
	// Proveri da li račun pripada klijentu.
	var count int64
	if err := r.db.WithContext(ctx).
		Table("core_banking.racun").
		Where("id = ? AND id_vlasnika = ? AND status = 'AKTIVAN'", input.RacunID, vlasnikID).
		Count(&count).Error; err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, domain.ErrAccountNotFound
	}

	sortCol := "vreme_izvrsavanja"
	if input.SortBy == "tip" {
		sortCol = "tip_transakcije"
	}
	sortOrder := "DESC"
	if input.SortOrder == "ASC" {
		sortOrder = "ASC"
	}

	var models []transakcijaModel
	err := r.db.WithContext(ctx).
		Where("racun_id = ?", input.RacunID).
		Order(fmt.Sprintf("%s %s", sortCol, sortOrder)).
		Find(&models).Error
	if err != nil {
		return nil, err
	}

	result := make([]domain.Transakcija, 0, len(models))
	for _, m := range models {
		result = append(result, domain.Transakcija{
			ID:               m.ID,
			RacunID:          m.RacunID,
			TipTransakcije:   m.TipTransakcije,
			Iznos:            m.Iznos,
			Opis:             m.Opis,
			VremeIzvrsavanja: m.VremeIzvrsavanja,
			Status:           m.Status,
		})
	}
	return result, nil
}

// RenameAccount menja naziv računa uz validaciju vlasništva i jedinstvenosti naziva.
func (r *accountRepository) RenameAccount(ctx context.Context, input domain.RenameAccountInput) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var racun racunModel
		if err := tx.Where("id = ? AND id_vlasnika = ? AND status = 'AKTIVAN'", input.AccountID, input.VlasnikID).
			First(&racun).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return domain.ErrAccountNotFound
			}
			return err
		}

		if racun.NazivRacuna == input.NoviNaziv {
			return domain.ErrNazivIsti
		}

		var dupCount int64
		if err := tx.Table("core_banking.racun").
			Where("id_vlasnika = ? AND naziv_racuna = ? AND id != ?", input.VlasnikID, input.NoviNaziv, input.AccountID).
			Count(&dupCount).Error; err != nil {
			return err
		}
		if dupCount > 0 {
			return domain.ErrNazivVecPostoji
		}

		return tx.Model(&racunModel{}).
			Where("id = ?", input.AccountID).
			Update("naziv_racuna", input.NoviNaziv).Error
	})
}

// pendingActionModel je GORM projekcija tabele pending_action.
type pendingActionModel struct {
	ID               int64     `gorm:"column:id;primaryKey;autoIncrement"`
	VlasnikID        int64     `gorm:"column:vlasnik_id"`
	RacunID          int64     `gorm:"column:racun_id"`
	ActionType       string    `gorm:"column:action_type"`
	ParamsJSON       string    `gorm:"column:params_json"`
	Opis             string    `gorm:"column:opis"`
	Status           string    `gorm:"column:status"`
	VerificationCode *string   `gorm:"column:verification_code"`
	CodeExpiresAt    *time.Time `gorm:"column:code_expires_at"`
	Attempts         int       `gorm:"column:attempts"`
	CreatedAt        time.Time `gorm:"column:created_at"`
}

func (pendingActionModel) TableName() string { return "core_banking.pending_action" }

// pendingActionListRow za JOIN sa racun, valuta i (opciono) payment_intent.
type pendingActionListRow struct {
	ID                 int64     `gorm:"column:id"`
	ActionType         string    `gorm:"column:action_type"`
	Opis               string    `gorm:"column:opis"`
	ParamsJSON         string    `gorm:"column:params_json"`
	Status             string    `gorm:"column:status"`
	BrojRacuna         string    `gorm:"column:broj_racuna"`
	ValutaOznaka       string    `gorm:"column:valuta_oznaka"`
	CreatedAt          time.Time `gorm:"column:created_at"`
	NazivPrimaoca      string    `gorm:"column:naziv_primaoca"`
	BrojRacunaPrimaoca string    `gorm:"column:broj_racuna_primaoca"`
	Iznos              float64   `gorm:"column:iznos"`
}

// UpdateAccountLimit kreira pending action za promenu limita; ne primenjuje promenu odmah.
func (r *accountRepository) UpdateAccountLimit(ctx context.Context, input domain.UpdateLimitInput) (int64, error) {
	// Proveri da li račun postoji i pripada vlasniku.
	var row struct {
		BrojRacuna   string `gorm:"column:broj_racuna"`
		ValutaOznaka string `gorm:"column:valuta_oznaka"`
	}
	err := r.db.WithContext(ctx).Raw(`
		SELECT ra.broj_racuna, v.oznaka AS valuta_oznaka
		FROM core_banking.racun ra
		JOIN core_banking.valuta v ON v.id = ra.id_valute
		WHERE ra.id = ? AND ra.id_vlasnika = ? AND ra.status = 'AKTIVAN'
		LIMIT 1
	`, input.AccountID, input.VlasnikID).Scan(&row).Error
	if err != nil {
		return 0, err
	}
	if row.BrojRacuna == "" {
		return 0, domain.ErrAccountNotFound
	}

	// Blokiraj duplikat: jedan aktivan PENDING zahtev za promenu limita po računu.
	var existingCount int64
	if err := r.db.WithContext(ctx).Model(&pendingActionModel{}).
		Where("racun_id = ? AND action_type = 'PROMENA_LIMITA' AND status = 'PENDING'", input.AccountID).
		Count(&existingCount).Error; err != nil {
		return 0, err
	}
	if existingCount > 0 {
		return 0, domain.ErrPendingAlreadyExists
	}

	// Serijalizuj parametre kao JSON. -1 je sentinel "ne menjaj taj limit".
	paramsBytes, _ := json.Marshal(limitParams{DnevniLimit: input.DnevniLimit, MesecniLimit: input.MesecniLimit})
	params := string(paramsBytes)

	// Opis opisuje samo ono što se stvarno menja.
	var opisParts []string
	if input.DnevniLimit >= 0 {
		opisParts = append(opisParts, fmt.Sprintf("Dnevni: %.2f %s", input.DnevniLimit, row.ValutaOznaka))
	}
	if input.MesecniLimit >= 0 {
		opisParts = append(opisParts, fmt.Sprintf("Mesečni: %.2f %s", input.MesecniLimit, row.ValutaOznaka))
	}
	opis := "Promena limita — " + strings.Join(opisParts, " / ")

	m := &pendingActionModel{
		VlasnikID:  input.VlasnikID,
		RacunID:    input.AccountID,
		ActionType: "PROMENA_LIMITA",
		ParamsJSON: params,
		Opis:       opis,
		Status:     "PENDING",
	}
	if err := r.db.WithContext(ctx).Create(m).Error; err != nil {
		return 0, err
	}
	return m.ID, nil
}

// GetPendingActions vraća sve PENDING akcije vlasnika.
func (r *accountRepository) GetPendingActions(ctx context.Context, vlasnikID int64) ([]domain.PendingAction, error) {
	var rows []pendingActionListRow
	err := r.db.WithContext(ctx).Raw(`
		SELECT
			pa.id,
			pa.action_type,
			pa.opis,
			pa.params_json,
			pa.status,
			ra.broj_racuna,
			v.oznaka AS valuta_oznaka,
			pa.created_at,
			COALESCE(pi.naziv_primaoca, '')   AS naziv_primaoca,
			COALESCE(pi.broj_racuna_primaoca, '') AS broj_racuna_primaoca,
			COALESCE(pi.iznos, 0)             AS iznos
		FROM core_banking.pending_action pa
		JOIN core_banking.racun ra ON ra.id = pa.racun_id
		JOIN core_banking.valuta v ON v.id = ra.id_valute
		LEFT JOIN core_banking.payment_intent pi ON pi.pending_action_id = pa.id
		WHERE pa.vlasnik_id = ? AND pa.status = 'PENDING'
		ORDER BY pa.created_at DESC
	`, vlasnikID).Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	result := make([]domain.PendingAction, 0, len(rows))
	for _, row := range rows {
		pa := parsePendingActionRow(row)
		result = append(result, pa)
	}
	return result, nil
}

// GetPendingAction vraća jednu pending akciju po ID-u; greška ako ne pripada vlasniku.
func (r *accountRepository) GetPendingAction(ctx context.Context, actionID, vlasnikID int64) (*domain.PendingAction, error) {
	var row pendingActionListRow
	err := r.db.WithContext(ctx).Raw(`
		SELECT
			pa.id,
			pa.action_type,
			pa.opis,
			pa.params_json,
			pa.status,
			ra.broj_racuna,
			v.oznaka AS valuta_oznaka,
			pa.created_at,
			COALESCE(pi.naziv_primaoca, '')   AS naziv_primaoca,
			COALESCE(pi.broj_racuna_primaoca, '') AS broj_racuna_primaoca,
			COALESCE(pi.iznos, 0)             AS iznos
		FROM core_banking.pending_action pa
		JOIN core_banking.racun ra ON ra.id = pa.racun_id
		JOIN core_banking.valuta v ON v.id = ra.id_valute
		LEFT JOIN core_banking.payment_intent pi ON pi.pending_action_id = pa.id
		WHERE pa.id = ? AND pa.vlasnik_id = ?
		LIMIT 1
	`, actionID, vlasnikID).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == 0 {
		return nil, domain.ErrPendingNotFound
	}
	pa := parsePendingActionRow(row)
	return &pa, nil
}

// ApprovePendingAction generiše 6-cifreni verifikacioni kod (važeći 5 min).
func (r *accountRepository) ApprovePendingAction(ctx context.Context, actionID, vlasnikID int64) (string, time.Time, error) {
	var m pendingActionModel
	if err := r.db.WithContext(ctx).
		Where("id = ? AND vlasnik_id = ?", actionID, vlasnikID).
		First(&m).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", time.Time{}, domain.ErrPendingNotFound
		}
		return "", time.Time{}, err
	}
	if m.Status != "PENDING" {
		return "", time.Time{}, domain.ErrAlreadyApproved
	}

	// Generiši 6-cifreni kod.
	code := fmt.Sprintf("%06d", (time.Now().UnixNano()%900000)+100000)
	expiresAt := time.Now().UTC().Add(5 * time.Minute)

	if err := r.db.WithContext(ctx).
		Model(&pendingActionModel{}).
		Where("id = ?", actionID).
		Updates(map[string]interface{}{
			"verification_code": code,
			"code_expires_at":   expiresAt,
			"status":            "APPROVED",
		}).Error; err != nil {
		return "", time.Time{}, err
	}
	return code, expiresAt, nil
}

// VerifyAndApplyLimit proverava kod i primenjuje promenu limita.
func (r *accountRepository) VerifyAndApplyLimit(ctx context.Context, input domain.VerifyLimitInput) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var m pendingActionModel
		if err := tx.Where("id = ? AND vlasnik_id = ?", input.ActionID, input.VlasnikID).
			First(&m).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return domain.ErrPendingNotFound
			}
			return err
		}

		if m.Status != "APPROVED" {
			if m.Status == "CANCELLED" {
				return domain.ErrTooManyAttempts
			}
			return domain.ErrPendingNotFound
		}

		// Proveri isticanje.
		if m.CodeExpiresAt == nil || time.Now().UTC().After(*m.CodeExpiresAt) {
			_ = tx.Model(&pendingActionModel{}).Where("id = ?", input.ActionID).
				Update("status", "CANCELLED").Error
			return domain.ErrCodeExpired
		}

		// Proveri kod.
		if m.VerificationCode == nil || *m.VerificationCode != input.Code {
			newAttempts := m.Attempts + 1
			update := map[string]interface{}{"attempts": newAttempts}
			if newAttempts >= 3 {
				update["status"] = "CANCELLED"
			}
			_ = tx.Model(&pendingActionModel{}).Where("id = ?", input.ActionID).Updates(update).Error
			if newAttempts >= 3 {
				return domain.ErrTooManyAttempts
			}
			return domain.ErrWrongCode
		}

		// Parsiraj parametre iz JSON-a koristeći encoding/json.
		var p limitParams
		if err := json.Unmarshal([]byte(m.ParamsJSON), &p); err != nil {
			return fmt.Errorf("neispravni parametri akcije: %w", err)
		}
		dnevniLimit := p.DnevniLimit
		mesecniLimit := p.MesecniLimit

		// Primeni promenu limita. -1 je sentinel "ne menjaj taj limit".
		updates := map[string]interface{}{}
		if dnevniLimit >= 0 {
			updates["dnevni_limit"] = dnevniLimit
		}
		if mesecniLimit >= 0 {
			updates["mesecni_limit"] = mesecniLimit
		}
		if len(updates) > 0 {
			result := tx.Model(&racunModel{}).
				Where("id = ? AND status = 'AKTIVAN'", m.RacunID).
				Updates(updates)
			if result.Error != nil {
				return result.Error
			}
		}

		// Označi akciju kao završenu.
		return tx.Model(&pendingActionModel{}).
			Where("id = ?", input.ActionID).
			Update("status", "CANCELLED").Error // reuse CANCELLED to mark "done"
	})
}

// parsePendingActionRow konvertuje row u domain.PendingAction.
func parsePendingActionRow(row pendingActionListRow) domain.PendingAction {
	// Koristimo encoding/json umesto fmt.Sscanf jer Sscanf ne parsira negativne
	// brojeve pouzdano u ovom kontekstu (-1 sentinel vrednost).
	var p limitParams
	_ = json.Unmarshal([]byte(row.ParamsJSON), &p)
	return domain.PendingAction{
		ID:                 row.ID,
		ActionType:         row.ActionType,
		Opis:               row.Opis,
		Status:             row.Status,
		BrojRacuna:         row.BrojRacuna,
		ValutaOznaka:       row.ValutaOznaka,
		DnevniLimit:        p.DnevniLimit,
		MesecniLimit:       p.MesecniLimit,
		CreatedAt:          row.CreatedAt,
		NazivPrimaoca:      row.NazivPrimaoca,
		BrojRacunaPrimaoca: row.BrojRacunaPrimaoca,
		Iznos:              row.Iznos,
	}
}
