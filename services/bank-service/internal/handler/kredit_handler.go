package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	pb "banka-backend/proto/banka"
	auth "banka-backend/shared/auth"
	"banka-backend/services/bank-service/internal/domain"
	"banka-backend/services/bank-service/internal/worker"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// extractEmployeeID čita korisnički ID iz JWT claims-a i proverava da je EMPLOYEE ili ADMIN.
func extractEmployeeID(ctx context.Context) (int64, error) {
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok || (claims.UserType != "EMPLOYEE" && claims.UserType != "ADMIN") {
		return 0, status.Error(codes.PermissionDenied, "samo zaposleni mogu pristupiti ovom resursu")
	}
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		return 0, status.Errorf(codes.Internal, "neispravan korisnički ID u tokenu: %v", err)
	}
	return id, nil
}

// kreditToPb konvertuje domain.Kredit u pb.Credit.
func kreditToPb(k domain.Kredit) *pb.Credit {
	pb := &pb.Credit{
		Id:                    k.ID,
		BrojKredita:           k.BrojKredita,
		BrojRacuna:            k.BrojRacuna,
		VrstaKredita:          k.VrstaKredita,
		IznosKredita:          fmt.Sprintf("%.2f", k.IznosKredita),
		PeriodOtplate:         k.PeriodOtplate,
		NominalnaKamatnaStopa: fmt.Sprintf("%.4f", k.NominalnaKamatnaStopa),
		EfektivnaKamatnaStopa: fmt.Sprintf("%.4f", k.EfektivnaKamatnaStopa),
		DatumUgovaranja:       k.DatumUgovaranja.Format("2006-01-02"),
		IznosMesecneRate:      fmt.Sprintf("%.2f", k.IznosMesecneRate),
		PreostaloDugovanje:    fmt.Sprintf("%.2f", k.PreostaloDugovanje),
		Valuta:                k.Valuta,
		Status:                k.Status,
		TipKamate:             k.TipKamate,
	}
	if k.DatumIsplate != nil {
		pb.DatumIsplate = k.DatumIsplate.Format("2006-01-02")
	}
	if k.DatumSledeceRate != nil {
		pb.DatumSledeceRate = k.DatumSledeceRate.Format("2006-01-02")
	}
	return pb
}

// rataToPb konvertuje domain.Rata u pb.Installment.
func rataToPb(r domain.Rata) *pb.Installment {
	pb := &pb.Installment{
		Id:                    r.ID,
		KreditId:              r.KreditID,
		IznosRate:             fmt.Sprintf("%.2f", r.IznosRate),
		IznosKamatneStope:     fmt.Sprintf("%.2f", r.IznosKamate),
		Valuta:                r.Valuta,
		OcekivaniDatumDospeca: r.OcekivaniDatumDospeca.Format("2006-01-02"),
		StatusPlacanja:        r.StatusPlacanja,
	}
	if r.PraviDatumDospeca != nil {
		pb.PraviDatumDospeca = r.PraviDatumDospeca.Format("2006-01-02")
	}
	return pb
}

// zahtevToPb konvertuje domain.KreditniZahtev u pb.CreditRequest.
func zahtevToPb(z domain.KreditniZahtev) *pb.CreditRequest {
	return &pb.CreditRequest{
		Id:                z.ID,
		KlijentId:         z.VlasnikID,
		VrstaKredita:      z.VrstaKredita,
		TipKamate:         z.TipKamate,
		IznosKredita:      fmt.Sprintf("%.2f", z.IznosKredita),
		Valuta:            z.Valuta,
		SvrhaKredita:      z.SvrhaKredita,
		IznosMesecnePlate: fmt.Sprintf("%.2f", z.IznosMesecnePlate),
		StatusZaposlenja:  z.StatusZaposlenja,
		PeriodZaposlenja:  z.PeriodZaposlenja,
		KontaktTelefon:    z.KontaktTelefon,
		BrojRacuna:        z.BrojRacuna,
		RokOtplate:        z.RokOtplate,
		DatumPodnosenja:   z.DatumPodnosenja.Format(time.RFC3339),
		Status:            z.Status,
	}
}

// parseOptionalFloat parsira string iznos; vraća 0.0 i nil grešku za prazan string.
func parseOptionalFloat(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseFloat(s, 64)
}

// =============================================================================
// RPC metode — Klijent
// =============================================================================

// ApplyForCredit podnosi zahtev za novi kredit.
// Mapped to: POST /api/v1/client/credits
func (h *BankHandler) ApplyForCredit(
	ctx context.Context,
	req *pb.ApplyForCreditRequest,
) (*pb.ApplyForCreditResponse, error) {
	vlasnikID, err := extractClientID(ctx)
	if err != nil {
		return nil, err
	}

	iznosKredita, err := parseOptionalFloat(req.IznosKredita)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "neispravan iznos kredita: %v", err)
	}
	iznosMesecnePlate, err := parseOptionalFloat(req.IznosMesecnePlate)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "neispravan iznos plate: %v", err)
	}

	input := domain.CreateKreditniZahtevInput{
		VlasnikID:         vlasnikID,
		VrstaKredita:      req.VrstaKredita,
		TipKamate:         req.TipKamate,
		IznosKredita:      iznosKredita,
		Valuta:            req.Valuta,
		SvrhaKredita:      req.SvrhaKredita,
		IznosMesecnePlate: iznosMesecnePlate,
		StatusZaposlenja:  req.StatusZaposlenja,
		PeriodZaposlenja:  req.PeriodZaposlenja,
		KontaktTelefon:    req.KontaktTelefon,
		BrojRacuna:        req.BrojRacuna,
		RokOtplate:        req.RokOtplate,
	}

	zahtev, err := h.kreditService.ApplyForCredit(ctx, input)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "greška pri podnošenju zahteva: %v", err)
	}

	// Pošalji email potvrdu da je zahtev primljen (fire-and-forget).
	if h.userClient != nil {
		if email, mailErr := h.userClient.GetMyEmail(ctx); mailErr == nil && email != "" {
			if pubErr := h.accountPublisher.Publish(worker.AccountEmailEvent{
				Type:  worker.KreditPodnetType,
				Email: email,
				Token: "",
			}); pubErr != nil {
				log.Printf("[apply-credit] UPOZORENJE: zahtev kreiran (id=%d) ali KREDIT_PODNET email nije poslat: %v", zahtev.ID, pubErr)
			}
		}
	}

	return &pb.ApplyForCreditResponse{
		Id:             zahtev.ID,
		Status:         zahtev.Status,
		DatumPodnosenja: zahtev.DatumPodnosenja.Format(time.RFC3339),
	}, nil
}

// GetClientCredits vraća sve kredite prijavljenog klijenta.
// Mapped to: GET /api/v1/client/credits
func (h *BankHandler) GetClientCredits(
	ctx context.Context,
	_ *emptypb.Empty,
) (*pb.GetClientCreditsResponse, error) {
	vlasnikID, err := extractClientID(ctx)
	if err != nil {
		return nil, err
	}

	krediti, err := h.kreditService.GetClientCredits(ctx, vlasnikID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "greška pri dohvatu kredita: %v", err)
	}

	pbKrediti := make([]*pb.Credit, 0, len(krediti))
	for _, k := range krediti {
		pbKrediti = append(pbKrediti, kreditToPb(k))
	}

	return &pb.GetClientCreditsResponse{Credits: pbKrediti}, nil
}

// GetCreditDetails vraća detalje i rate za jedan kredit.
// Mapped to: GET /api/v1/client/credits/{id}
func (h *BankHandler) GetCreditDetails(
	ctx context.Context,
	req *pb.GetCreditDetailsRequest,
) (*pb.GetCreditDetailsResponse, error) {
	vlasnikID, err := extractClientID(ctx)
	if err != nil {
		return nil, err
	}

	kredit, rate, err := h.kreditService.GetCreditDetails(ctx, req.Id, vlasnikID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrKreditNotFound):
			return nil, status.Error(codes.NotFound, "kredit nije pronađen")
		case errors.Is(err, domain.ErrKreditForbidden):
			return nil, status.Error(codes.PermissionDenied, "kredit ne pripada trenutnom korisniku")
		default:
			return nil, status.Errorf(codes.Internal, "greška pri dohvatu detalja: %v", err)
		}
	}

	pbRate := make([]*pb.Installment, 0, len(rate))
	for _, r := range rate {
		pbRate = append(pbRate, rataToPb(r))
	}

	return &pb.GetCreditDetailsResponse{
		Kredit: kreditToPb(*kredit),
		Rate:   pbRate,
	}, nil
}

// =============================================================================
// RPC metode — Zaposleni
// =============================================================================

// GetAllCreditRequests vraća sve zahteve sa opcionim filterima.
// Mapped to: GET /api/v1/employee/credits/requests
func (h *BankHandler) GetAllCreditRequests(
	ctx context.Context,
	req *pb.GetAllCreditRequestsRequest,
) (*pb.GetAllCreditRequestsResponse, error) {
	if _, err := extractEmployeeID(ctx); err != nil {
		return nil, err
	}

	filter := domain.GetPendingRequestsFilter{
		VrstaKredita: req.VrstaKredita,
		BrojRacuna:   req.BrojRacuna,
	}

	zahtevi, err := h.kreditService.GetAllPendingRequests(ctx, filter)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "greška pri dohvatu zahteva: %v", err)
	}

	pbZahtevi := make([]*pb.CreditRequest, 0, len(zahtevi))
	for _, z := range zahtevi {
		pbZahtevi = append(pbZahtevi, zahtevToPb(z))
	}

	return &pb.GetAllCreditRequestsResponse{Zahtevi: pbZahtevi}, nil
}

// ApproveCredit odobrava zahtev za kredit i kredituje račun klijenta.
// Mapped to: POST /api/v1/employee/credits/requests/{id}/approve
func (h *BankHandler) ApproveCredit(
	ctx context.Context,
	req *pb.ApproveCreditRequest,
) (*pb.ApproveCreditResponse, error) {
	if _, err := extractEmployeeID(ctx); err != nil {
		return nil, err
	}

	kredit, err := h.kreditService.ApproveCredit(ctx, req.Id)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrKreditniZahtevNotFound):
			return nil, status.Error(codes.NotFound, "zahtev za kredit nije pronađen")
		case errors.Is(err, domain.ErrZahtevVecObrađen):
			return nil, status.Error(codes.FailedPrecondition, "zahtev je već obrađen")
		default:
			return nil, status.Errorf(codes.Internal, "greška pri odobravanju kredita: %v", err)
		}
	}

	// Pokušaj naplatu prve rate odmah po odobravanju (fire-and-forget za email).
	insufficientFunds, _, installErr := h.kreditService.ProcessFirstInstallment(ctx, kredit.ID)
	if installErr != nil {
		log.Printf("[approve-credit] UPOZORENJE: kredit odobren (id=%d) ali naplata prve rate nije uspela: %v", kredit.ID, installErr)
	}
	if insufficientFunds && h.userClient != nil {
		// Nema dovoljno sredstava — pošalji upozorenje klijentu emailom.
		if email, mailErr := h.userClient.GetClientEmail(ctx, kredit.VlasnikID); mailErr == nil && email != "" {
			if pubErr := h.accountPublisher.Publish(worker.AccountEmailEvent{
				Type:  worker.KreditRataUpozorenjeType,
				Email: email,
				Token: "",
			}); pubErr != nil {
				log.Printf("[approve-credit] UPOZORENJE: kredit odobren (id=%d) ali KREDIT_RATA_UPOZORENJE email nije poslat: %v", kredit.ID, pubErr)
			}
		}
	}

	return &pb.ApproveCreditResponse{Kredit: kreditToPb(*kredit)}, nil
}

// RejectCredit odbija zahtev za kredit.
// Mapped to: POST /api/v1/employee/credits/requests/{id}/reject
func (h *BankHandler) RejectCredit(
	ctx context.Context,
	req *pb.RejectCreditRequest,
) (*emptypb.Empty, error) {
	if _, err := extractEmployeeID(ctx); err != nil {
		return nil, err
	}

	if err := h.kreditService.RejectCredit(ctx, req.Id); err != nil {
		switch {
		case errors.Is(err, domain.ErrKreditniZahtevNotFound):
			return nil, status.Error(codes.NotFound, "zahtev za kredit nije pronađen")
		case errors.Is(err, domain.ErrZahtevVecObrađen):
			return nil, status.Error(codes.FailedPrecondition, "zahtev je već obrađen")
		default:
			return nil, status.Errorf(codes.Internal, "greška pri odbijanju kredita: %v", err)
		}
	}

	return &emptypb.Empty{}, nil
}

// GetAllApprovedCredits vraća sve odobrene kredite sa filterima.
// Mapped to: GET /api/v1/employee/credits
func (h *BankHandler) GetAllApprovedCredits(
	ctx context.Context,
	req *pb.GetAllApprovedCreditsRequest,
) (*pb.GetAllApprovedCreditsResponse, error) {
	if _, err := extractEmployeeID(ctx); err != nil {
		return nil, err
	}

	filter := domain.GetAllCreditsFilter{
		VrstaKredita: req.VrstaKredita,
		BrojRacuna:   req.BrojRacuna,
		Status:       req.Status,
	}

	krediti, err := h.kreditService.GetAllApprovedCredits(ctx, filter)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "greška pri dohvatu kredita: %v", err)
	}

	pbKrediti := make([]*pb.Credit, 0, len(krediti))
	for _, k := range krediti {
		pbKrediti = append(pbKrediti, kreditToPb(k))
	}

	return &pb.GetAllApprovedCreditsResponse{Krediti: pbKrediti}, nil
}
