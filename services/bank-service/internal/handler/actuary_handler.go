package handler

import (
	"context"
	"errors"
	"strconv"

	pb "banka-backend/proto/actuary"
	auth "banka-backend/shared/auth"
	"banka-backend/services/bank-service/internal/domain"

	"github.com/shopspring/decimal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// ActuaryHandler implementira pb.ActuaryServiceServer.
type ActuaryHandler struct {
	pb.UnimplementedActuaryServiceServer
	service domain.ActuaryService
}

func NewActuaryHandler(service domain.ActuaryService) *ActuaryHandler {
	return &ActuaryHandler{service: service}
}

// ─── Mapiranje domain → proto ─────────────────────────────────────────────────

func toProtoActuaryType(t domain.ActuaryType) pb.ActuaryType {
	switch t {
	case domain.ActuaryTypeSupervisor:
		return pb.ActuaryType_ACTUARY_TYPE_SUPERVISOR
	case domain.ActuaryTypeAgent:
		return pb.ActuaryType_ACTUARY_TYPE_AGENT
	default:
		return pb.ActuaryType_ACTUARY_TYPE_UNSPECIFIED
	}
}

// toProtoActuaryInfo maps a domain.Actuary to pb.ActuaryInfo.
// Limit and UsedLimit are transported as proto double (float64). The precision
// is maintained at the domain/DB level (decimal.Decimal / NUMERIC); the small
// loss of precision on the wire is acceptable given proto has no decimal type.
func toProtoActuaryInfo(a *domain.Actuary) *pb.ActuaryInfo {
	limitF, _ := a.Limit.Float64()
	usedLimitF, _ := a.UsedLimit.Float64()
	return &pb.ActuaryInfo{
		Id:           a.ID,
		EmployeeId:   a.EmployeeID,
		ActuaryType:  toProtoActuaryType(a.ActuaryType),
		Limit:        limitF,
		UsedLimit:    usedLimitF,
		NeedApproval: a.NeedApproval,
	}
}

// ─── Helpers voor autentikaciju ───────────────────────────────────────────────

// extractActuaryEmployeeID reads the employee ID from JWT claims.
func extractActuaryEmployeeID(ctx context.Context) (int64, error) {
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return 0, status.Error(codes.Unauthenticated, "nedostaju JWT claims")
	}
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		return 0, status.Errorf(codes.Internal, "neispravan ID u tokenu: %v", err)
	}
	return id, nil
}

// requireSupervisor checks that the caller is ADMIN or has SUPERVISOR permission in their JWT.
func (h *ActuaryHandler) requireSupervisor(ctx context.Context) error {
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "nedostaju JWT claims")
	}
	// ADMIN always has access.
	if claims.UserType == "ADMIN" {
		return nil
	}
	// For employees, check the SUPERVISOR permission in the JWT.
	for _, p := range claims.Permissions {
		if p == "SUPERVISOR" {
			return nil
		}
	}
	return status.Error(codes.PermissionDenied, domain.ErrNotSupervisor.Error())
}

// ─── RPC: GetMyActuaryInfo ────────────────────────────────────────────────────

func (h *ActuaryHandler) GetMyActuaryInfo(ctx context.Context, _ *emptypb.Empty) (*pb.GetMyActuaryInfoResponse, error) {
	employeeID, err := extractActuaryEmployeeID(ctx)
	if err != nil {
		return nil, err
	}
	a, err := h.service.GetActuaryByEmployeeID(ctx, employeeID)
	if errors.Is(err, domain.ErrActuaryNotFound) {
		return nil, status.Error(codes.NotFound, domain.ErrNotActuary.Error())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "greška pri dohvatu aktuara: %v", err)
	}
	return &pb.GetMyActuaryInfoResponse{Actuary: toProtoActuaryInfo(a)}, nil
}

// ─── RPC: GetActuaryByEmployeeID ──────────────────────────────────────────────

func (h *ActuaryHandler) GetActuaryByEmployeeID(ctx context.Context, req *pb.GetActuaryByEmployeeIDRequest) (*pb.GetActuaryByEmployeeIDResponse, error) {
	if err := h.requireSupervisor(ctx); err != nil {
		return nil, err
	}
	a, err := h.service.GetActuaryByEmployeeID(ctx, req.EmployeeId)
	if errors.Is(err, domain.ErrActuaryNotFound) {
		return nil, status.Error(codes.NotFound, domain.ErrActuaryNotFound.Error())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "greška pri dohvatu aktuara: %v", err)
	}
	return &pb.GetActuaryByEmployeeIDResponse{Actuary: toProtoActuaryInfo(a)}, nil
}

// ─── RPC: GetAgents ───────────────────────────────────────────────────────────

// GetAgents vraća listu agenata (aktuary_type = AGENT) za supervizorski portal.
// NOTE: Polja email, first_name, last_name i position nisu popunjena —
// to su cross-service atributi iz user-service-a. Pozivalac treba da ih
// obogati pozivom user-service-a koristeći employee_id iz svakog reda.
func (h *ActuaryHandler) GetAgents(ctx context.Context, _ *pb.GetAgentsRequest) (*pb.GetAgentsResponse, error) {
	if err := h.requireSupervisor(ctx); err != nil {
		return nil, err
	}
	agents, err := h.service.ListAgents(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "greška pri dohvatu agenata: %v", err)
	}
	items := make([]*pb.AgentListItem, 0, len(agents))
	for i := range agents {
		limitF, _ := agents[i].Limit.Float64()
		usedF, _ := agents[i].UsedLimit.Float64()
		items = append(items, &pb.AgentListItem{
			Id:           agents[i].ID,
			EmployeeId:   agents[i].EmployeeID,
			Limit:        limitF,
			UsedLimit:    usedF,
			NeedApproval: agents[i].NeedApproval,
		})
	}
	return &pb.GetAgentsResponse{Agents: items}, nil
}

// ─── RPC: SetAgentLimit ───────────────────────────────────────────────────────

func (h *ActuaryHandler) SetAgentLimit(ctx context.Context, req *pb.SetAgentLimitRequest) (*emptypb.Empty, error) {
	if err := h.requireSupervisor(ctx); err != nil {
		return nil, err
	}
	// Convert proto double → decimal to preserve precision in the domain layer.
	limit := decimal.NewFromFloat(req.Limit)
	_, err := h.service.SetAgentLimit(ctx, req.EmployeeId, limit)
	if errors.Is(err, domain.ErrActuaryNotFound) {
		return nil, status.Error(codes.NotFound, domain.ErrActuaryNotFound.Error())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "greška pri postavljanju limita: %v", err)
	}
	return &emptypb.Empty{}, nil
}

// ─── RPC: ResetAgentUsedLimit ─────────────────────────────────────────────────

func (h *ActuaryHandler) ResetAgentUsedLimit(ctx context.Context, req *pb.ResetAgentUsedLimitRequest) (*emptypb.Empty, error) {
	if err := h.requireSupervisor(ctx); err != nil {
		return nil, err
	}
	_, err := h.service.ResetAgentUsedLimit(ctx, req.EmployeeId)
	if errors.Is(err, domain.ErrActuaryNotFound) {
		return nil, status.Error(codes.NotFound, domain.ErrActuaryNotFound.Error())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "greška pri resetovanju limita: %v", err)
	}
	return &emptypb.Empty{}, nil
}

// ─── RPC: SetAgentNeedApproval ────────────────────────────────────────────────

func (h *ActuaryHandler) SetAgentNeedApproval(ctx context.Context, req *pb.SetAgentNeedApprovalRequest) (*emptypb.Empty, error) {
	if err := h.requireSupervisor(ctx); err != nil {
		return nil, err
	}
	_, err := h.service.SetAgentNeedApproval(ctx, req.EmployeeId, req.NeedApproval)
	if errors.Is(err, domain.ErrActuaryNotFound) {
		return nil, status.Error(codes.NotFound, domain.ErrActuaryNotFound.Error())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "greška pri postavljanju need_approval: %v", err)
	}
	return &emptypb.Empty{}, nil
}

// ─── RPC: CreateAgent ─────────────────────────────────────────────────────────

func (h *ActuaryHandler) CreateAgent(ctx context.Context, req *pb.CreateAgentRequest) (*pb.CreateAgentResponse, error) {
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "nedostaju JWT claims")
	}
	if claims.UserType != "ADMIN" {
		return nil, status.Error(codes.PermissionDenied, "pristup odbijen: zahteva ADMIN ulogu")
	}
	if req.EmployeeId == 0 {
		return nil, status.Error(codes.InvalidArgument, "employee_id je obavezan")
	}

	// Reject if the target employee already has a SUPERVISOR actuary record.
	existing, err := h.service.GetActuaryByEmployeeID(ctx, req.EmployeeId)
	if err == nil && existing.ActuaryType == domain.ActuaryTypeSupervisor {
		return nil, status.Error(codes.InvalidArgument, "supervizori ne mogu biti dodati kao agenti")
	}

	a, err := h.service.CreateActuaryForEmployee(ctx, req.EmployeeId, domain.ActuaryTypeAgent)
	if errors.Is(err, domain.ErrActuaryAlreadyExists) {
		return nil, status.Error(codes.AlreadyExists, domain.ErrActuaryAlreadyExists.Error())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "greška pri kreiranju agenta: %v", err)
	}

	if req.Limit > 0 {
		limit := decimal.NewFromFloat(req.Limit)
		a, err = h.service.SetAgentLimit(ctx, req.EmployeeId, limit)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "agent kreiran ali greška pri postavljanju limita: %v", err)
		}
	}

	return &pb.CreateAgentResponse{Actuary: toProtoActuaryInfo(a)}, nil
}
