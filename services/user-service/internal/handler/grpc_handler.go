// Package handler — gRPC delivery layer.
// Implements pb.UserServiceServer generated from proto/user/user.proto.
//
// Prerequisites before this package compiles:
//   - Run `make proto`        → generates banka-backend/proto/user (pb package)
//   - Run `sqlc generate`     → generates banka-backend/services/user-service/internal/database/sqlc
package handler

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	pb "banka-backend/proto/user"
	auth "banka-backend/shared/auth"
	db "banka-backend/services/user-service/internal/database/sqlc"
	"banka-backend/services/user-service/internal/domain"
	"banka-backend/services/user-service/internal/utils"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// UserHandler implements pb.UserServiceServer.
// Embedding UnimplementedUserServiceServer satisfies the interface for any RPC
// methods not yet overridden and provides forward-compatibility when new RPCs
// are added to the proto.
type UserHandler struct {
	pb.UnimplementedUserServiceServer
	querier          db.Querier           // injected sqlc query layer (non-transactional reads)
	sqlDB            *sql.DB              // raw connection pool — used only to open transactions
	accessSecret     string               // HMAC secret for signing access tokens
	refreshSecret    string               // HMAC secret for signing refresh tokens
	activationSecret string               // HMAC secret for signing activation/reset tokens
	publisher             utils.EmailPublisher        // abstracts RabbitMQ publishing for testability
	userCreatedPublisher  utils.UserCreatedPublisher  // publishes employee-created events to bank-service
	clientSvc             domain.ClientService        // use-case layer for client operations
	bankClient            BankActuaryClient           // nil → bank-service sync disabled
}

// NewUserHandler constructs a UserHandler.
// sqlDB is required for handlers that need multi-statement transactions.
// publisher handles async email event dispatch; inject utils.NewAMQPPublisher in production.
// userCreatedPublisher notifies bank-service of new employees; inject utils.NewAMQPUserCreatedPublisher in production.
// clientSvc handles client use-case logic; inject service.NewClientService in production.
// bankClient syncs actuary records; pass nil to disable (e.g. if BANK_SERVICE_ADDR is unset).
func NewUserHandler(q db.Querier, sqlDB *sql.DB, accessSecret, refreshSecret, activationSecret string, publisher utils.EmailPublisher, userCreatedPublisher utils.UserCreatedPublisher, clientSvc domain.ClientService, bankClient BankActuaryClient) *UserHandler {
	return &UserHandler{
		querier:              q,
		sqlDB:                sqlDB,
		accessSecret:         accessSecret,
		refreshSecret:        refreshSecret,
		activationSecret:     activationSecret,
		publisher:            publisher,
		userCreatedPublisher: userCreatedPublisher,
		clientSvc:            clientSvc,
		bankClient:           bankClient,
	}
}

// extractBearerToken reads the raw token string from gRPC incoming metadata.
// Returns "" if the header is absent or malformed.
func extractBearerToken(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return ""
	}
	return strings.TrimPrefix(vals[0], "Bearer ")
}

// ─── Health ───────────────────────────────────────────────────────────────────

// HealthCheck responds to both liveness probes and the gRPC-Gateway GET /health
// route defined in the proto HTTP annotation.
func (h *UserHandler) HealthCheck(_ context.Context, _ *pb.HealthCheckRequest) (*pb.HealthCheckResponse, error) {
	return &pb.HealthCheckResponse{Status: "SERVING"}, nil
}

// ─── Employee management (Admin only) ────────────────────────────────────────

// CreateEmployee creates a new employee account without a password.
//
// Flow (all steps run inside a single DB transaction):
//  1. ADMIN-only guard via JWT claims.
//  2. Validate mandatory fields (email, first_name, last_name).
//  3. INSERT into users — password_hash/salt_password left empty for activation.
//  4. INSERT into employee_details — username derived from first.last if omitted.
//  5. Loop through requested permissions and INSERT into user_permissions.
//  6. COMMIT; return the new user ID + email.
//
// NOTE: notification/activation-email trigger is deferred to a later phase.
// Mapped to: POST /employee
func (h *UserHandler) CreateEmployee(ctx context.Context, req *pb.CreateEmployeeRequest) (*pb.CreateEmployeeResponse, error) {
	// ── 1. Authorization — ADMIN only ─────────────────────────────────────────
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok || claims.UserType != "ADMIN" {
		return nil, status.Errorf(codes.PermissionDenied, "only administrators can create employees")
	}

	// ── 2. Mandatory field validation ─────────────────────────────────────────
	switch {
	case strings.TrimSpace(req.Email) == "":
		return nil, status.Errorf(codes.InvalidArgument, "email is required")
	case strings.TrimSpace(req.FirstName) == "":
		return nil, status.Errorf(codes.InvalidArgument, "first_name is required")
	case strings.TrimSpace(req.LastName) == "":
		return nil, status.Errorf(codes.InvalidArgument, "last_name is required")
	case req.BirthDate > 0 && req.BirthDate > time.Now().UnixMilli():
		return nil, status.Errorf(codes.InvalidArgument, "birth date cannot be in the future")
	case !isValidPhone(req.PhoneNumber):
		return nil, status.Errorf(codes.InvalidArgument, "phone number may only contain digits and an optional leading +")
	}

	// ── 3. Open transaction ───────────────────────────────────────────────────
	tx, err := h.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to begin transaction")
	}
	// Rollback is a no-op once Commit succeeds.
	defer tx.Rollback() //nolint:errcheck

	qtx := db.New(tx) // transaction-bound query layer

	// ── 4. is_active default ──────────────────────────────────────────────────
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}

	// ── 5. INSERT users ───────────────────────────────────────────────────────
	// Resolve the user_type: default to EMPLOYEE when unspecified.
	userType := userTypeToString(req.UserType)
	if userType == "" {
		userType = "EMPLOYEE"
	}

	newUser, err := qtx.CreateUser(ctx, db.CreateUserParams{
		Email:        strings.TrimSpace(req.Email),
		PasswordHash: "", // set during account activation (Issue 11)
		SaltPassword: "", // set during account activation (Issue 11)
		UserType:     userType,
		FirstName:    strings.TrimSpace(req.FirstName),
		LastName:     strings.TrimSpace(req.LastName),
		BirthDate:    req.BirthDate,
		Gender:       nullStrIf(genderToString(req.Gender), req.Gender != pb.Gender_GENDER_UNSPECIFIED),
		PhoneNumber:  nullStrIf(req.PhoneNumber, req.PhoneNumber != ""),
		Address:      nullStrIf(req.Address, req.Address != ""),
		IsActive:     isActive,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, status.Errorf(codes.AlreadyExists, "email already registered: %s", req.Email)
		}
		return nil, status.Errorf(codes.Internal, "failed to create user")
	}

	// ── 6. INSERT employee_details ────────────────────────────────────────────
	// Username falls back to "firstname.lastname" (lowercase) when not supplied.
	username := strings.TrimSpace(req.Username)
	if username == "" {
		username = strings.ToLower(req.FirstName + "." + req.LastName)
	}

	position := strings.TrimSpace(req.Position)
	if userType == "ADMIN" {
		position = "Administrator"
	}

	err = qtx.CreateEmployeeDetails(ctx, db.CreateEmployeeDetailsParams{
		UserID:     newUser.ID,
		Username:   username,
		Position:   nullStrIf(position, position != ""),
		Department: nullStrIf(req.Department, req.Department != ""),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, status.Errorf(codes.AlreadyExists, "username already taken: %s", username)
		}
		return nil, status.Errorf(codes.Internal, "failed to create employee details")
	}

	// ── 7. Assign permissions ─────────────────────────────────────────────────
	// Build the effective permission list by starting from whatever the caller
	// supplied, then auto-deriving SUPERVISOR / AGENT from the employee's
	// position and always granting SUPERVISOR to ADMIN accounts.
	effectivePerms := make([]string, 0, len(req.Permissions)+1)
	effectivePerms = append(effectivePerms, req.Permissions...)

	switch strings.TrimSpace(req.Position) {
	case "Supervizor":
		effectivePerms = appendIfMissing(effectivePerms, PermSupervisor)
	case "Agent":
		effectivePerms = appendIfMissing(effectivePerms, PermAgent)
	}

	// ADMINs always carry SUPERVISOR in the DB so that bank-service can
	// recognise them as actuaries via the user_created event.
	if userType == "ADMIN" {
		effectivePerms = appendIfMissing(effectivePerms, PermSupervisor)
	}

	for _, code := range effectivePerms {
		if strings.TrimSpace(code) == "" {
			continue
		}
		if err := qtx.AssignUserPermission(ctx, db.AssignUserPermissionParams{
			UserID:         newUser.ID,
			PermissionCode: code,
		}); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to assign permission %q", code)
		}
	}

	// ── 8. Commit ─────────────────────────────────────────────────────────────
	if err := tx.Commit(); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to commit transaction")
	}

	// ── 9. Publish activation event ───────────────────────────────────────────
	// The user is already committed; any messaging failure must NOT roll back
	// the DB or return an error to the caller — the frontend would have no way
	// to reconcile a committed row against a gRPC error response.
	token, err := auth.GenerateActivationToken(req.Email, h.activationSecret)
	if err != nil {
		log.Printf("[create-employee] failed to generate activation token for %s: %v", req.Email, err)
	} else if err := h.publisher.Publish(utils.EmailEvent{
		Type:  "ACTIVATION",
		Email: req.Email,
		Token: token,
	}); err != nil {
		log.Printf("[create-employee] failed to publish activation event for %s: %v", req.Email, err)
	}

	// Notify bank-service so it can auto-provision an actuary profile for
	// SUPERVISOR and AGENT employees. Use effectivePerms (not req.Permissions)
	// so auto-derived and ADMIN permissions are included. Fire-and-forget.
	if err := h.userCreatedPublisher.Publish(utils.UserCreatedEvent{
		EmployeeID:  newUser.ID,
		Email:       newUser.Email,
		Permissions: effectivePerms,
	}); err != nil {
		log.Printf("[create-employee] failed to publish user-created event for employee %d: %v", newUser.ID, err)
	}

	return &pb.CreateEmployeeResponse{
		Id:    newUser.ID,
		Email: newUser.Email,
	}, nil
}

// ─── Employee update (Issue 8) ────────────────────────────────────────────────

// UpdateEmployee replaces all mutable fields of an existing employee and
// atomically swaps their full permission set.
//
// Authorization: caller must be ADMIN or hold MANAGE_USERS permission.
//
// Flow:
//  1. Auth guard.
//  2. Validate mandatory fields (id, email, first_name, last_name).
//  3. Pre-fetch target user — NOT_FOUND if missing; PERMISSION_DENIED if ADMIN.
//  4. Begin transaction.
//  5. UpdateUser — all mutable base columns including is_active.
//  6. UpdateEmployeeDetails — position, department.
//  7. DeleteUserPermissions — wipe old set.
//  8. AssignUserPermission — re-assign each code in req.Permissions.
//  9. Commit; read back fresh profile and return.
//
// Mapped to: PUT /employee/{id}
func (h *UserHandler) UpdateEmployee(ctx context.Context, req *pb.UpdateEmployeeRequest) (*pb.UpdateEmployeeResponse, error) {
	// ── 1. Authorization ──────────────────────────────────────────────────────
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.PermissionDenied, "missing authentication claims")
	}
	isAdmin := claims.UserType == "ADMIN"
	hasManageUsers := slices.Contains(claims.Permissions, "MANAGE_USERS")
	if !isAdmin && !hasManageUsers {
		return nil, status.Errorf(codes.PermissionDenied, "only admins or users with MANAGE_USERS permission can update employees")
	}

	// Extract the raw Bearer token so we can forward it to bank-service later.
	bearerToken := extractBearerToken(ctx)

	// ── 2. Mandatory field validation ─────────────────────────────────────────
	switch {
	case req.Id == 0:
		return nil, status.Errorf(codes.InvalidArgument, "id is required")
	case strings.TrimSpace(req.Email) == "":
		return nil, status.Errorf(codes.InvalidArgument, "email is required")
	case strings.TrimSpace(req.FirstName) == "":
		return nil, status.Errorf(codes.InvalidArgument, "first_name is required")
	case strings.TrimSpace(req.LastName) == "":
		return nil, status.Errorf(codes.InvalidArgument, "last_name is required")
	case req.BirthDate > 0 && req.BirthDate > time.Now().UnixMilli():
		return nil, status.Errorf(codes.InvalidArgument, "birth date cannot be in the future")
	case !isValidPhone(req.PhoneNumber):
		return nil, status.Errorf(codes.InvalidArgument, "phone number may only contain digits and an optional leading +")
	}

	// ── 2a. Detect admin-promotion sentinel ────────────────────────────────────
	// The frontend signals "promote to ADMIN" by including ADMIN_PERMISSION in the
	// permissions array. When detected: change user_type to ADMIN, assign all DB
	// permissions + force SUPERVISOR, skip AGENT/SUPERVISOR mutex check.
	makingAdmin := slices.Contains(req.Permissions, PermAdmin)

	// ── 2b. AGENT ↔ SUPERVISOR mutual exclusivity (non-admin updates only) ────
	hasSupervisorInReq := slices.Contains(req.Permissions, PermSupervisor)
	hasAgentInReq      := slices.Contains(req.Permissions, PermAgent)
	if !makingAdmin && hasSupervisorInReq && hasAgentInReq {
		return nil, status.Errorf(codes.InvalidArgument, "zaposleni ne može imati istovremeno AGENT i SUPERVISOR permisiju")
	}

	// ── 3. Pre-fetch target (not found + admin guard) ─────────────────────────
	existing, err := h.querier.GetEmployeeByID(ctx, req.Id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "employee not found")
		}
		return nil, status.Errorf(codes.Internal, "failed to fetch employee")
	}
	if existing.UserType == "ADMIN" {
		return nil, status.Errorf(codes.PermissionDenied, "admin accounts cannot be edited")
	}

	// ── 3a. Determine old actuary role (for change-detection) ─────────────────
	oldPerms, err := h.querier.GetUserPermissions(ctx, req.Id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to read existing permissions")
	}
	oldHasSupervisor := slices.Contains(oldPerms, PermSupervisor)
	oldHasAgent      := slices.Contains(oldPerms, PermAgent)

	// New effective actuary type after this update.
	newActuaryType := ""
	if makingAdmin || hasSupervisorInReq {
		newActuaryType = "SUPERVISOR"
	} else if hasAgentInReq {
		newActuaryType = "AGENT"
	}
	oldActuaryType := ""
	if oldHasSupervisor {
		oldActuaryType = "SUPERVISOR"
	} else if oldHasAgent {
		oldActuaryType = "AGENT"
	}

	// ── 4. Begin transaction ──────────────────────────────────────────────────
	tx, err := h.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to begin transaction")
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := db.New(tx)

	// ── 5a. Promote to ADMIN: update user_type ────────────────────────────────
	if makingAdmin {
		if _, err := tx.ExecContext(ctx, "UPDATE users SET user_type = 'ADMIN' WHERE id = $1", req.Id); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to set user_type to ADMIN")
		}
	}

	// ── 5b. UpdateUser (base fields) ─────────────────────────────────────────
	if err := qtx.UpdateUser(ctx, db.UpdateUserParams{
		ID:          req.Id,
		Email:       strings.TrimSpace(req.Email),
		FirstName:   strings.TrimSpace(req.FirstName),
		LastName:    strings.TrimSpace(req.LastName),
		BirthDate:   req.BirthDate,
		Gender:      nullStrIf(genderToString(req.Gender), req.Gender != pb.Gender_GENDER_UNSPECIFIED),
		PhoneNumber: nullStrIf(req.PhoneNumber, req.PhoneNumber != ""),
		Address:     nullStrIf(req.Address, req.Address != ""),
		IsActive:    req.IsActive,
	}); err != nil {
		if isUniqueViolation(err) {
			return nil, status.Errorf(codes.AlreadyExists, "email already in use: %s", req.Email)
		}
		return nil, status.Errorf(codes.Internal, "failed to update user")
	}

	// ── 6. UpdateEmployeeDetails ──────────────────────────────────────────────
	if err := qtx.UpdateEmployeeDetails(ctx, db.UpdateEmployeeDetailsParams{
		UserID:     req.Id,
		Position:   nullStrIf(req.Position, req.Position != ""),
		Department: nullStrIf(req.Department, req.Department != ""),
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to update employee details")
	}

	// ── 7. DeleteUserPermissions ──────────────────────────────────────────────
	if err := qtx.DeleteUserPermissions(ctx, req.Id); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to clear permissions")
	}

	// ── 8. Re-assign permissions ──────────────────────────────────────────────
	// For admin promotion: fetch all permissions from DB and assign them all
	// (including ADMIN_PERMISSION itself and SUPERVISOR).
	// For regular update: use req.Permissions but skip the ADMIN_PERMISSION sentinel.
	permsToAssign := req.Permissions
	if makingAdmin {
		allPerms, err := h.querier.GetAllPermissions(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to load permission codebook")
		}
		permsToAssign = make([]string, 0, len(allPerms)+1)
		for _, p := range allPerms {
			permsToAssign = append(permsToAssign, p.PermissionCode)
		}
		permsToAssign = appendIfMissing(permsToAssign, PermSupervisor)
	}

	for _, code := range permsToAssign {
		code = strings.TrimSpace(code)
		if code == "" || code == PermAdmin {
			continue // skip the sentinel; ADMIN_PERMISSION is stored separately for real admins
		}
		if err := qtx.AssignUserPermission(ctx, db.AssignUserPermissionParams{
			UserID:         req.Id,
			PermissionCode: code,
		}); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to assign permission %q", code)
		}
	}

	// ── 9. Sync actuary record in bank-service (before commit for rollback) ───
	// Determine whether we need to create, delete, or re-type the actuary record.
	if h.bankClient != nil {
		needDelete := oldActuaryType != "" && (newActuaryType == "" || newActuaryType != oldActuaryType)
		needCreate := newActuaryType != "" && (oldActuaryType == "" || newActuaryType != oldActuaryType)

		if needDelete {
			if err := h.bankClient.DeleteActuary(ctx, req.Id, bearerToken); err != nil {
				log.Printf("[UpdateEmployee] bank-service DeleteActuary employee_id=%d: %v", req.Id, err)
				return nil, status.Errorf(codes.Internal, "failed to remove actuary record in bank-service: %v", err)
			}
		}
		if needCreate {
			if err := h.bankClient.CreateActuary(ctx, req.Id, newActuaryType, bearerToken); err != nil {
				log.Printf("[UpdateEmployee] bank-service CreateActuary employee_id=%d type=%s: %v", req.Id, newActuaryType, err)
				return nil, status.Errorf(codes.Internal, "failed to provision actuary record in bank-service: %v", err)
			}
		}
	}

	// ── 10. Commit ────────────────────────────────────────────────────────────
	if err := tx.Commit(); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to commit transaction")
	}

	// Read back fresh profile to include the updated permission set.
	updated, err := h.querier.GetEmployeeByID(ctx, req.Id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to read updated employee")
	}
	perms, err := h.querier.GetUserPermissions(ctx, req.Id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to read updated permissions")
	}

	return &pb.UpdateEmployeeResponse{
		Employee: &pb.EmployeeProfile{
			User: &pb.User{
				Id:          updated.ID,
				Email:       updated.Email,
				FirstName:   updated.FirstName,
				LastName:    updated.LastName,
				BirthDate:   updated.BirthDate,
				Gender:      genderFromString(updated.Gender),
				PhoneNumber: fromNullStr(updated.PhoneNumber),
				Address:     fromNullStr(updated.Address),
				UserType:    userTypeFromString(updated.UserType),
				IsActive:    updated.IsActive,
				CreatedAt:   timestamppb.New(updated.CreatedAt),
			},
			Username:    updated.Username,
			Position:    fromNullStr(updated.Position),
			Department:  fromNullStr(updated.Department),
			Permissions: perms,
		},
	}, nil
}

// ─── Toggle employee active (deactivate/activate any user including ADMIN) ───

// ToggleEmployeeActive sets is_active for the given user (EMPLOYEE or ADMIN).
// Authorization: ADMIN or MANAGE_USERS. Does not require fetching full profile,
// so it works for ADMIN accounts that cannot be edited via UpdateEmployee.
//
// Mapped to: PATCH /employee/{id}/active (after make proto).
func (h *UserHandler) ToggleEmployeeActive(ctx context.Context, req *pb.ToggleEmployeeActiveRequest) (*pb.ToggleEmployeeActiveResponse, error) {
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.PermissionDenied, "missing authentication claims")
	}
	isAdmin := claims.UserType == "ADMIN"
	hasManageUsers := false
	for _, p := range claims.Permissions {
		if p == "MANAGE_USERS" {
			hasManageUsers = true
			break
		}
	}
	if !isAdmin && !hasManageUsers {
		return nil, status.Errorf(codes.PermissionDenied, "only admins or users with MANAGE_USERS permission can toggle active status")
	}

	if req.Id == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "id is required")
	}

	user, err := h.querier.GetUserByID(ctx, req.Id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "user not found")
		}
		return nil, status.Errorf(codes.Internal, "failed to fetch user")
	}

	// Only allow toggling employees and admins, not clients
	if user.UserType != "ADMIN" && user.UserType != "EMPLOYEE" {
		return nil, status.Errorf(codes.PermissionDenied, "can only toggle active status for employees and administrators")
	}

	if err := h.querier.UpdateUserActive(ctx, db.UpdateUserActiveParams{
		ID:       req.Id,
		IsActive: req.IsActive,
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to update active status")
	}

	return &pb.ToggleEmployeeActiveResponse{IsActive: req.IsActive}, nil
}

// ─── Account activation / set password (Issue 11) ────────────────────────────

// SetPassword lets an employee set their initial password by presenting the
// activation JWT they received by e-mail.
//
// Flow (no auth header required — the activation token IS the credential):
//  1. Validate password length (min 8 characters).
//  2. Verify the activation JWT — Unauthenticated if invalid or expired.
//  3. Fetch the user by email — NotFound if the account no longer exists.
//  4. Replay-attack guard: if password_hash is already set the token has been
//     consumed; return FailedPrecondition so the link cannot be reused.
//  5. Hash the new password with bcrypt.
//  6. Persist the password_hash.
//
// Mapped to: POST /auth/set-password
func (h *UserHandler) SetPassword(ctx context.Context, req *pb.SetPasswordRequest) (*pb.SetPasswordResponse, error) {
	// ── 1. Password validation ────────────────────────────────────────────────
	if len(req.Password) < 8 {
		return nil, status.Errorf(codes.InvalidArgument, "password must be at least 8 characters")
	}

	// ── 2. Verify activation token ────────────────────────────────────────────
	email, err := auth.VerifyActivationToken(req.Token, h.activationSecret)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid or expired activation token")
	}

	// ── 3. Fetch user ─────────────────────────────────────────────────────────
	user, err := h.querier.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "user not found")
		}
		return nil, status.Errorf(codes.Internal, "failed to fetch user")
	}

	// ── 4. Replay-attack guard ────────────────────────────────────────────────
	// A non-empty password_hash means the account was already activated.
	// The activation token must be treated as single-use: once the password
	// is set any further use of the same token (valid for 24 h) is rejected.
	if user.PasswordHash != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "account is already activated and password is set; token is no longer valid")
	}

	// ── 5. Hash password ──────────────────────────────────────────────────────
	hashed, err := utils.HashPassword(req.Password)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to hash password")
	}

	// ── 6. Persist ────────────────────────────────────────────────────────────
	if err := h.querier.UpdateUserPassword(ctx, db.UpdateUserPasswordParams{
		PasswordHash: hashed,
		Email:        email,
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to update password")
	}

	return &pb.SetPasswordResponse{Message: "Password set successfully."}, nil
}

// ActivateAccount lets an employee set their password via the activation token
// received by e-mail.
//
// Flow (no auth header required — the activation token IS the credential):
//  1. Validate passwords match and meet minimum length.
//  2. Verify the activation JWT — Unauthenticated if invalid or expired.
//  3. Fetch the user by email — NotFound if the account no longer exists.
//  4. Replay-attack guard: if password_hash is already set the token has been
//     consumed; return FailedPrecondition so the link cannot be reused.
//  5. Hash the new password with bcrypt.
//  6. Persist the password_hash.
//
// Mapped to: POST /activate
func (h *UserHandler) ActivateAccount(ctx context.Context, req *pb.ActivateAccountRequest) (*pb.ActivateAccountResponse, error) {
	// ── 1. Password validation ────────────────────────────────────────────────
	if req.NewPassword != req.ConfirmPassword {
		return nil, status.Errorf(codes.InvalidArgument, "passwords do not match")
	}
	if len(req.NewPassword) < 8 {
		return nil, status.Errorf(codes.InvalidArgument, "password must be at least 8 characters")
	}

	// ── 2. Verify activation token ────────────────────────────────────────────
	email, err := auth.VerifyActivationToken(req.Token, h.activationSecret)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid or expired activation token")
	}

	// ── 3. Fetch user ─────────────────────────────────────────────────────────
	user, err := h.querier.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "user not found")
		}
		return nil, status.Errorf(codes.Internal, "failed to fetch user")
	}

	// ── 4. Replay-attack guard ────────────────────────────────────────────────
	if user.PasswordHash != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "account is already activated; token is no longer valid")
	}

	// ── 5. Hash password ──────────────────────────────────────────────────────
	hashed, err := utils.HashPassword(req.NewPassword)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to hash password")
	}

	// ── 6. Persist ────────────────────────────────────────────────────────────
	if err := h.querier.UpdateUserPassword(ctx, db.UpdateUserPasswordParams{
		PasswordHash: hashed,
		Email:        email,
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to update password")
	}

	// ── 7. Send confirmation email ────────────────────────────────────────────
	// Fire-and-forget: password is already committed, messaging failure must not
	// surface as a gRPC error — the account is activated regardless.
	if err := h.publisher.Publish(utils.EmailEvent{
		Type:  "ACTIVATION_SUCCESS",
		Email: email,
		Token: "",
	}); err != nil {
		log.Printf("[activate-account] failed to publish confirmation event for %s: %v", email, err)
	}

	return &pb.ActivateAccountResponse{Message: "Account activated successfully."}, nil
}

// ─── Authentication ───────────────────────────────────────────────────────────

// Login validates email + password and returns a JWT access/refresh token pair.
//
// Flow:
//  1. Fetch user row by email — NOT_FOUND if email is unknown.
//  2. Reject inactive accounts — PERMISSION_DENIED.
//  3. Verify bcrypt password hash — UNAUTHENTICATED on mismatch.
//  4. Fetch the user's permission codes from user_permissions.
//  5. Issue access token (15 min) and refresh token (7 days).
//
// Mapped to: POST /login
func (h *UserHandler) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {
	// ── 1. Lookup ─────────────────────────────────────────────────────────────
	user, err := h.querier.GetUserByEmail(ctx, req.Email)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			log.Printf("[login] user not found for email=%q", req.Email)
			return nil, status.Errorf(codes.NotFound, "no account found for that email")
		}
		log.Printf("[login] DB error fetching user: %v", err)
		return nil, status.Errorf(codes.Internal, "database error during login")
	}

	// ── 2. Active check ───────────────────────────────────────────────────────
	if !user.IsActive {
		log.Printf("[login] account inactive: user_id=%d email=%q", user.ID, user.Email)
		return nil, status.Errorf(codes.PermissionDenied, "account is inactive")
	}

	// ── 3. Password verification ──────────────────────────────────────────────
	// Comparing req.Password against user.PasswordHash only — SaltPassword is
	// NOT appended because bcrypt embeds its own salt inside the hash itself.
	if err := utils.CheckPassword(req.Password, user.PasswordHash); err != nil {
		log.Printf("[login] bcrypt mismatch: user_id=%d hash_len=%d err=%v",
			user.ID, len(user.PasswordHash), err)
		return nil, status.Errorf(codes.Unauthenticated, "invalid credentials")
	}

	// ── 4. Permissions ────────────────────────────────────────────────────────
	perms, err := h.querier.GetUserPermissions(ctx, user.ID)
	if err != nil {
		log.Printf("[login] failed to load permissions: user_id=%d err=%v", user.ID, err)
		return nil, status.Errorf(codes.Internal, "failed to load permissions")
	}

	// ── 5. Token generation ───────────────────────────────────────────────────
	// Access token:  { sub, email, user_type, permissions[], iat, exp }
	// Refresh token: { sub, iat, exp, token_type: "refresh" }
	userIDStr := strconv.FormatInt(user.ID, 10)
	accessToken, refreshToken, err := auth.GenerateTokens(
		userIDStr,
		user.Email,
		user.UserType,
		perms,
		h.accessSecret,
		h.refreshSecret,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to generate tokens")
	}

	return &pb.LoginResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    900, // 15 minutes in seconds
	}, nil
}

// ─── Token refresh ────────────────────────────────────────────────────────────

// RefreshToken validates a refresh token and issues a new access token.
// The original refresh token is returned unchanged — no rolling sessions.
//
// Flow:
//  1. Verify the refresh token signature + expiry; reject if it carries token_type != "refresh".
//  2. Extract user ID from the sub claim and look the user up — NOT_FOUND if gone.
//  3. Reject inactive accounts — PERMISSION_DENIED.
//  4. Fetch fresh permissions so the new access token reflects any RBAC changes.
//  5. Sign a new access token (15 min); echo the original refresh token back.
//
// Mapped to: POST /refresh-token
func (h *UserHandler) RefreshToken(ctx context.Context, req *pb.RefreshTokenRequest) (*pb.RefreshTokenResponse, error) {
	// ── 1. Verify refresh token ───────────────────────────────────────────────
	refreshClaims, err := auth.VerifyRefreshToken(req.RefreshToken, h.refreshSecret)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid refresh token: %v", err)
	}

	// ── 2. User freshness check ───────────────────────────────────────────────
	userIDInt, err := strconv.ParseInt(refreshClaims.Subject, 10, 64)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "malformed subject claim")
	}

	user, err := h.querier.GetUserByID(ctx, userIDInt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "user no longer exists")
		}
		return nil, status.Errorf(codes.Internal, "database error during token refresh")
	}

	// ── 3. Active check ───────────────────────────────────────────────────────
	if !user.IsActive {
		return nil, status.Errorf(codes.PermissionDenied, "account is inactive")
	}

	// ── 4. Fresh permissions ──────────────────────────────────────────────────
	perms, err := h.querier.GetUserPermissions(ctx, user.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to load permissions")
	}

	// ── 5. Issue new access token only ────────────────────────────────────────
	userIDStr := strconv.FormatInt(user.ID, 10)
	accessToken, err := auth.GenerateAccessToken(
		userIDStr,
		user.Email,
		user.UserType,
		perms,
		h.accessSecret,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to generate access token")
	}

	return &pb.RefreshTokenResponse{
		AccessToken:  accessToken,
		RefreshToken: req.RefreshToken, // original token, unchanged
		TokenType:    "Bearer",
		ExpiresIn:    900, // 15 minutes
	}, nil
}

// ─── Permission codebook (Issue 5) ───────────────────────────────────────────

// GetAllPermissions returns every row from the permissions table so the admin
// frontend can populate Create/Edit Employee permission checkboxes.
//
// Authorization: ADMIN only.
// Mapped to: GET /permissions
func (h *UserHandler) GetAllPermissions(ctx context.Context, _ *pb.GetAllPermissionsRequest) (*pb.GetAllPermissionsResponse, error) {
	// ── 1. Authorization — ADMIN only ─────────────────────────────────────────
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok || claims.UserType != "ADMIN" {
		return nil, status.Errorf(codes.PermissionDenied, "only administrators can list permissions")
	}

	// ── 2. Query ──────────────────────────────────────────────────────────────
	rows, err := h.querier.GetAllPermissions(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to fetch permissions")
	}

	// ── 3. Map & return ───────────────────────────────────────────────────────
	// Permisije koje se ne prikazuju u employee checkboxovima:
	// ADMIN_PERMISSION — implicitna za admina, ne dodeljuje se zaposlenima
	// TRADE_STOCKS     — klijentska permisija (nema UI dodeljivanja za klijente)
	// MANAGE_USERS     — admin-nivo kontrola, ne dodeljuje se zaposlenima
	nonEmployeePerms := map[string]bool{
		"ADMIN_PERMISSION": true,
		"TRADE_STOCKS":     true,
		"MANAGE_USERS":     true,
	}

	entries := make([]*pb.PermissionEntry, 0, len(rows))
	for _, r := range rows {
		if nonEmployeePerms[r.PermissionCode] {
			continue
		}
		entries = append(entries, &pb.PermissionEntry{
			Id:             int32(r.ID),
			PermissionCode: r.PermissionCode,
		})
	}

	return &pb.GetAllPermissionsResponse{Permissions: entries}, nil
}

// ─── Employee listing (Issue 6) ───────────────────────────────────────────────

// GetAllEmployees lists employees with optional partial-match filters and pagination.
//
// Flow:
//  1. Verify caller is ADMIN via JWT claims — PERMISSION_DENIED otherwise.
//  2. Normalise pagination defaults (page=1, size=10 when zero).
//  3. Map empty-string filters to sql.NullString{Valid:false} so the SQL
//     `IS NULL` branch fires and returns all employees (critical frontend edge case).
//  4. Query, map rows to EmployeeProfile, return.
//
// Mapped to: GET /employee
func (h *UserHandler) GetAllEmployees(ctx context.Context, req *pb.GetAllEmployeesRequest) (*pb.GetAllEmployeesResponse, error) {
	// ── 1. Authorization — ADMIN only ─────────────────────────────────────────
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok || claims.UserType != "ADMIN" {
		return nil, status.Errorf(codes.PermissionDenied, "only admins can list employees")
	}

	// ── 2. Pagination defaults ────────────────────────────────────────────────
	page := req.Page
	if page <= 0 {
		page = 1
	}
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = 10
	}
	offset := (page - 1) * pageSize

	// ── 3. Optional filter mapping ────────────────────────────────────────────
	// The frontend sends "" when a filter is cleared. An empty string must map
	// to sql.NullString{Valid:false} so the SQL `IS NULL` branch fires and the
	// filter is skipped entirely — returning all employees for that field.
	params := db.ListEmployeesParams{
		Limit:  pageSize,
		Offset: offset,
	}
	if req.Email != "" {
		params.Email = sql.NullString{String: req.Email, Valid: true}
	}
	if req.FirstName != "" {
		params.FirstName = sql.NullString{String: req.FirstName, Valid: true}
	}
	if req.LastName != "" {
		params.LastName = sql.NullString{String: req.LastName, Valid: true}
	}
	if req.Position != "" {
		params.Position = sql.NullString{String: req.Position, Valid: true}
	}

	// ── 4. Query & map ────────────────────────────────────────────────────────
	rows, err := h.querier.ListEmployees(ctx, params)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list employees")
	}

	employees := make([]*pb.EmployeeProfile, 0, len(rows))
	for _, row := range rows {
		employees = append(employees, &pb.EmployeeProfile{
			User: &pb.User{
				Id:          row.ID,
				Email:       row.Email,
				FirstName:   row.FirstName,
				LastName:    row.LastName,
				UserType:    userTypeFromString(row.UserType),
				IsActive:    row.IsActive,
				PhoneNumber: fromNullStr(row.PhoneNumber),
			},
			Position:   fromNullStr(row.Position),
			Department: fromNullStr(row.Department),
		})
	}

	return &pb.GetAllEmployeesResponse{Employees: employees}, nil
}

// ─── Employee detail (Issue 7) ────────────────────────────────────────────────

// GetEmployeeByID returns the full employee profile + permissions for the edit form.
//
// Authorization: caller must be ADMIN or hold the MANAGE_USERS permission.
//
// Flow:
//  1. Auth guard — PERMISSION_DENIED if caller is neither ADMIN nor MANAGE_USERS.
//  2. Fetch users JOIN employee_details — NOT_FOUND if no matching row.
//  3. Block ADMIN-type users from being returned through this endpoint (Issue 7 edge case).
//  4. Fetch permissions — empty slice is valid (new employee with no permissions yet).
//  5. Map to EmployeeProfile and return.
//
// Mapped to: GET /employee/{id}
func (h *UserHandler) GetEmployeeByID(ctx context.Context, req *pb.GetEmployeeByIDRequest) (*pb.GetEmployeeByIDResponse, error) {
	// ── 1. Authorization ──────────────────────────────────────────────────────
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.PermissionDenied, "missing authentication claims")
	}
	isAdmin := claims.UserType == "ADMIN"
	hasManageUsers := false
	hasSupervisor := false
	for _, p := range claims.Permissions {
		if p == "MANAGE_USERS" {
			hasManageUsers = true
		}
		if p == "SUPERVISOR" {
			hasSupervisor = true
		}
	}
	if !isAdmin && !hasManageUsers && !hasSupervisor {
		return nil, status.Errorf(codes.PermissionDenied, "only admins or users with MANAGE_USERS or SUPERVISOR permission can fetch employee details")
	}

	// ── 2. Fetch employee row ─────────────────────────────────────────────────
	row, err := h.querier.GetEmployeeByID(ctx, req.Id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "employee %d not found", req.Id)
		}
		return nil, status.Errorf(codes.Internal, "failed to fetch employee")
	}

	// ── 3. Block ADMIN-type targets (Issue 7 edge case) ───────────────────────
	// Admin accounts are managed outside this employee-edit flow.
	if row.UserType == "ADMIN" {
		return nil, status.Errorf(codes.PermissionDenied, "admin accounts cannot be edited through this endpoint")
	}

	// ── 4. Fetch permissions ──────────────────────────────────────────────────
	perms, err := h.querier.GetUserPermissions(ctx, row.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to fetch permissions")
	}

	// ── 5. Map and return ─────────────────────────────────────────────────────
	return &pb.GetEmployeeByIDResponse{
		Employee: &pb.EmployeeProfile{
			User: &pb.User{
				Id:          row.ID,
				Email:       row.Email,
				FirstName:   row.FirstName,
				LastName:    row.LastName,
				BirthDate:   row.BirthDate,
				Gender:      genderFromString(row.Gender),
				PhoneNumber: fromNullStr(row.PhoneNumber),
				Address:     fromNullStr(row.Address),
				UserType:    userTypeFromString(row.UserType),
				IsActive:    row.IsActive,
				CreatedAt:   timestamppb.New(row.CreatedAt),
			},
			Username:    row.Username,
			Position:    fromNullStr(row.Position),
			Department:  fromNullStr(row.Department),
			Permissions: perms,
		},
	}, nil
}

// ─── Client management ────────────────────────────────────────────────────────

// SearchClients returns a paginated list of client previews for Autocomplete
// and Infinite Scroll on the account-creation form.
//
// Flow:
//  1. EMPLOYEE-only guard via JWT claims.
//  2. Normalise pagination defaults (page=1, limit=10 when zero).
//  3. Map empty query to sql.NullString{Valid:false} so the SQL IS NULL branch
//     fires and all clients are returned (no text filter applied).
//  4. Fetch limit+1 rows — one extra to detect has_more without a COUNT query.
//  5. Trim the slice back to limit and set has_more accordingly.
//
// Mapped to: GET /client/search
func (h *UserHandler) SearchClients(ctx context.Context, req *pb.SearchClientsRequest) (*pb.SearchClientsResponse, error) {
	// ── 1. Authorization — EMPLOYEE only ──────────────────────────────────────
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok || claims.UserType != "EMPLOYEE" {
		return nil, status.Errorf(codes.PermissionDenied, "only employees can search clients")
	}

	// ── 2. Pagination defaults ────────────────────────────────────────────────
	page := req.Page
	if page <= 0 {
		page = 1
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	offset := (page - 1) * limit

	// ── 3. Query mapping ──────────────────────────────────────────────────────
	// An empty query string maps to sql.NullString{Valid:false} so the SQL
	// IS NULL branch fires and all clients are returned (no filter applied).
	params := db.SearchClientsParams{
		Limit:  limit + 1, // fetch one extra to detect has_more
		Offset: offset,
	}
	if req.Query != "" {
		params.Query = sql.NullString{String: req.Query, Valid: true}
	}

	// ── 4. Query DB ───────────────────────────────────────────────────────────
	rows, err := h.querier.SearchClients(ctx, params)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to search clients")
	}

	// ── 5. has_more detection & slice trimming ────────────────────────────────
	hasMore := false
	if int32(len(rows)) > limit {
		hasMore = true
		rows = rows[:limit]
	}

	// ── 6. Map & return ───────────────────────────────────────────────────────
	previews := make([]*pb.ClientPreview, 0, len(rows))
	for _, row := range rows {
		previews = append(previews, &pb.ClientPreview{
			Id:        row.ID,
			FirstName: row.FirstName,
			LastName:  row.LastName,
			Email:     row.Email,
		})
	}

	return &pb.SearchClientsResponse{
		Clients: previews,
		HasMore: hasMore,
	}, nil
}

// CreateClient registers a new bank client account without a password.
//
// Flow:
//  1. EMPLOYEE-only guard via JWT claims.
//  2. Validate mandatory fields (email, first_name, last_name).
//  3. INSERT into users with user_type = 'CLIENT'; leave password_hash empty.
//  4. Return the new user ID + email.
//  5. Publish activation event asynchronously (fire-and-forget).
//
// NOTE: client_details is not populated here; that is deferred to a later phase.
// Mapped to: POST /client
func (h *UserHandler) CreateClient(ctx context.Context, req *pb.CreateClientRequest) (*pb.CreateClientResponse, error) {
	// ── 1. Authorization — EMPLOYEE only ──────────────────────────────────────
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok || claims.UserType != "EMPLOYEE" {
		return nil, status.Errorf(codes.PermissionDenied, "only employees can register clients")
	}

	// ── 2. Mandatory field validation ─────────────────────────────────────────
	switch {
	case strings.TrimSpace(req.Email) == "":
		return nil, status.Errorf(codes.InvalidArgument, "email is required")
	case strings.TrimSpace(req.FirstName) == "":
		return nil, status.Errorf(codes.InvalidArgument, "first_name is required")
	case strings.TrimSpace(req.LastName) == "":
		return nil, status.Errorf(codes.InvalidArgument, "last_name is required")
	case req.BirthDate > 0 && req.BirthDate > time.Now().UnixMilli():
		return nil, status.Errorf(codes.InvalidArgument, "birth date cannot be in the future")
	case !isValidPhone(req.PhoneNumber):
		return nil, status.Errorf(codes.InvalidArgument, "phone number may only contain digits and an optional leading +")
	}

	// ── 3. INSERT users ───────────────────────────────────────────────────────
	// user_type is always CLIENT; password stays empty until the client
	// activates their account via the activation link.
	newUser, err := h.querier.CreateUser(ctx, db.CreateUserParams{
		Email:        strings.TrimSpace(req.Email),
		PasswordHash: "",
		SaltPassword: "",
		UserType:     "CLIENT",
		FirstName:    strings.TrimSpace(req.FirstName),
		LastName:     strings.TrimSpace(req.LastName),
		BirthDate:    req.BirthDate,
		Gender:       nullStrIf(genderToString(req.Gender), req.Gender != pb.Gender_GENDER_UNSPECIFIED),
		PhoneNumber:  nullStrIf(req.PhoneNumber, req.PhoneNumber != ""),
		Address:      nullStrIf(req.Address, req.Address != ""),
		IsActive:     true,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, status.Errorf(codes.AlreadyExists, "email already registered: %s", req.Email)
		}
		return nil, status.Errorf(codes.Internal, "failed to create client")
	}

	// ── 4. Publish activation event ───────────────────────────────────────────
	// Fire-and-forget: the user is already committed; a messaging failure must
	// NOT return an error to the caller — the client row exists regardless.
	token, err := auth.GenerateActivationToken(req.Email, h.activationSecret)
	if err != nil {
		log.Printf("[create-client] failed to generate activation token for %s: %v", req.Email, err)
	} else if err := h.publisher.Publish(utils.EmailEvent{
		Type:  "ACTIVATION",
		Email: req.Email,
		Token: token,
	}); err != nil {
		log.Printf("[create-client] failed to publish activation event for %s: %v", req.Email, err)
	}

	return &pb.CreateClientResponse{
		Id:    newUser.ID,
		Email: newUser.Email,
	}, nil
}

// GetClientByID returns the full profile of a single client for the employee portal.
//
// Authorization: EMPLOYEE only — this is a staff-facing endpoint.
//
// Flow:
//  1. EMPLOYEE-only guard via JWT claims.
//  2. Validate id is non-zero — InvalidArgument otherwise.
//  3. Delegate to clientSvc.GetClientByID — returns ErrClientNotFound when the ID
//     is unknown or the user_type is not CLIENT.
//  4. Map the domain entity to the proto response and return.
//
// Mapped to: GET /client/{id}
func (h *UserHandler) GetClientByID(ctx context.Context, req *pb.GetClientByIDRequest) (*pb.GetClientByIDResponse, error) {
	// ── 1. Authorization — EMPLOYEE only ──────────────────────────────────────
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok || claims.UserType != "EMPLOYEE" {
		return nil, status.Errorf(codes.PermissionDenied, "only employees can view client details")
	}

	// ── 2. Input validation ───────────────────────────────────────────────────
	if req.Id == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "id is required")
	}

	// ── 3. Delegate to service ────────────────────────────────────────────────
	client, err := h.clientSvc.GetClientByID(ctx, req.Id)
	if err != nil {
		if errors.Is(err, domain.ErrClientNotFound) {
			return nil, status.Errorf(codes.NotFound, "client %d not found", req.Id)
		}
		return nil, status.Errorf(codes.Internal, "failed to fetch client")
	}

	// ── 4. Map and return ─────────────────────────────────────────────────────
	return &pb.GetClientByIDResponse{
		Client: &pb.ClientDetail{
			Id:          client.ID,
			FirstName:   client.FirstName,
			LastName:    client.LastName,
			Email:       client.Email,
			PhoneNumber: client.PhoneNumber,
			Address:     client.Address,
			DateOfBirth: client.DateOfBirth,
			Gender:      genderFromString(sql.NullString{String: client.Gender, Valid: client.Gender != ""}),
		},
	}, nil
}

// ListClients returns a paginated, alphabetically-sorted list of clients for
// the employee management portal.
//
// Flow:
//  1. EMPLOYEE-only guard via JWT claims.
//  2. Map empty filter strings to the "no filter" sentinel (empty string).
//  3. Delegate to clientSvc.ListClients — handles pagination defaults and
//     the has_more detection via limit+1 fetch.
//  4. Map domain summaries to proto ClientListItem.
//
// Mapped to: GET /client
func (h *UserHandler) ListClients(ctx context.Context, req *pb.ListClientsRequest) (*pb.ListClientsResponse, error) {
	// ── 1. Authorization — EMPLOYEE or ADMIN ──────────────────────────────────
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok || (claims.UserType != "EMPLOYEE" && claims.UserType != "ADMIN") {
		return nil, status.Errorf(codes.PermissionDenied, "only employees can list clients")
	}

	// ── 2. Delegate to service (applies pagination defaults internally) ────────
	summaries, hasMore, err := h.clientSvc.ListClients(ctx, domain.ClientFilter{
		Name:   req.Name,
		Email:  req.Email,
		Limit:  req.Limit,
		Offset: req.Offset,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list clients")
	}

	// ── 3. Map to proto ───────────────────────────────────────────────────────
	items := make([]*pb.ClientListItem, 0, len(summaries))
	for _, s := range summaries {
		items = append(items, &pb.ClientListItem{
			Id:          s.ID,
			FirstName:   s.FirstName,
			LastName:    s.LastName,
			Email:       s.Email,
			PhoneNumber: s.PhoneNumber,
		})
	}

	return &pb.ListClientsResponse{
		Clients: items,
		HasMore: hasMore,
	}, nil
}

// UpdateClient partially updates the mutable fields of an existing client.
//
// Flow:
//  1. EMPLOYEE-only guard via JWT claims.
//  2. Validate id is non-zero.
//  3. Validate phone_number format when provided.
//  4. Delegate to clientSvc.UpdateClient with PATCH-style semantics
//     (empty string = keep existing value).
//  5. Map the updated domain entity to the proto response.
//
// Mapped to: PATCH /client/{id}
func (h *UserHandler) UpdateClient(ctx context.Context, req *pb.UpdateClientRequest) (*pb.UpdateClientResponse, error) {
	// ── 1. Authorization — EMPLOYEE only ──────────────────────────────────────
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok || claims.UserType != "EMPLOYEE" {
		return nil, status.Errorf(codes.PermissionDenied, "only employees can update client details")
	}

	// ── 2. Input validation ───────────────────────────────────────────────────
	if req.Id == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "id is required")
	}
	if req.PhoneNumber != "" && !isValidPhone(req.PhoneNumber) {
		return nil, status.Errorf(codes.InvalidArgument, "phone_number must contain only digits and an optional leading +")
	}

	// ── 3. Delegate to service ────────────────────────────────────────────────
	updated, err := h.clientSvc.UpdateClient(ctx, req.Id, domain.UpdateClientInput{
		FirstName:   req.FirstName,
		LastName:    req.LastName,
		Email:       req.Email,
		PhoneNumber: req.PhoneNumber,
		Address:     req.Address,
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrClientNotFound):
			return nil, status.Errorf(codes.NotFound, "client %d not found", req.Id)
		case errors.Is(err, domain.ErrEmailTaken):
			return nil, status.Errorf(codes.AlreadyExists, "email is already in use")
		default:
			return nil, status.Errorf(codes.Internal, "failed to update client")
		}
	}

	// ── 4. Map and return ─────────────────────────────────────────────────────
	return &pb.UpdateClientResponse{
		Client: &pb.ClientDetail{
			Id:          updated.ID,
			FirstName:   updated.FirstName,
			LastName:    updated.LastName,
			Email:       updated.Email,
			PhoneNumber: updated.PhoneNumber,
			Address:     updated.Address,
			DateOfBirth: updated.DateOfBirth,
			Gender:      genderFromString(sql.NullString{String: updated.Gender, Valid: updated.Gender != ""}),
		},
	}, nil
}

// ─── Shared helpers ───────────────────────────────────────────────────────────

// isUniqueViolation reports whether err is a PostgreSQL unique-constraint
// violation (SQLSTATE 23505). Works with pgx/v5/stdlib errors.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// nullStrIf returns a valid sql.NullString when cond is true, NULL otherwise.
// Used to map optional proto string fields to nullable DB columns.
func nullStrIf(s string, cond bool) sql.NullString {
	return sql.NullString{String: s, Valid: cond}
}

// fromNullStr extracts the string value from a sql.NullString, returning "" when NULL.
func fromNullStr(ns sql.NullString) string {
	if ns.Valid {
		return ns.String
	}
	return ""
}

// genderFromString converts the nullable DB gender column to the proto Gender enum.
func genderFromString(ns sql.NullString) pb.Gender {
	if !ns.Valid {
		return pb.Gender_GENDER_UNSPECIFIED
	}
	switch ns.String {
	case "MALE":
		return pb.Gender_GENDER_MALE
	case "FEMALE":
		return pb.Gender_GENDER_FEMALE
	case "OTHER":
		return pb.Gender_GENDER_OTHER
	default:
		return pb.Gender_GENDER_UNSPECIFIED
	}
}

// genderToString converts the proto Gender enum to the DB VARCHAR value.
func genderToString(g pb.Gender) string {
	switch g {
	case pb.Gender_GENDER_MALE:
		return "MALE"
	case pb.Gender_GENDER_FEMALE:
		return "FEMALE"
	case pb.Gender_GENDER_OTHER:
		return "OTHER"
	default:
		return ""
	}
}

// userTypeFromString maps the DB user_type string to the proto enum.
func userTypeFromString(s string) pb.UserType {
	switch s {
	case "ADMIN":
		return pb.UserType_USER_TYPE_ADMIN
	case "EMPLOYEE":
		return pb.UserType_USER_TYPE_EMPLOYEE
	case "CLIENT":
		return pb.UserType_USER_TYPE_CLIENT
	default:
		return pb.UserType_USER_TYPE_UNSPECIFIED
	}
}

// userTypeToString maps the proto UserType enum to the DB VARCHAR value.
// Returns "" for UNSPECIFIED so the caller can apply a default.
func userTypeToString(t pb.UserType) string {
	switch t {
	case pb.UserType_USER_TYPE_ADMIN:
		return "ADMIN"
	case pb.UserType_USER_TYPE_EMPLOYEE:
		return "EMPLOYEE"
	case pb.UserType_USER_TYPE_CLIENT:
		return "CLIENT"
	default:
		return ""
	}
}

// isValidPhone reports whether s matches ^\+?[0-9]+$ (digits only, optional
// leading +). An empty string is also accepted because phone is an optional field.
func isValidPhone(s string) bool {
	if s == "" {
		return true
	}
	i := 0
	if s[0] == '+' {
		i = 1
	}
	if i >= len(s) {
		return false // bare "+" is invalid
	}
	for _, c := range s[i:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ─── Forgot / Reset password (Issue 12) ──────────────────────────────────────

// ForgotPassword triggers a mock password-reset notification.
//
// Security: ALWAYS returns the same success message regardless of whether the
// email exists — this prevents user-enumeration attacks. The token is only
// generated and logged when the account exists AND is active.
//
// Mapped to: POST /auth/forgot-password
func (h *UserHandler) ForgotPassword(ctx context.Context, req *pb.ForgotPasswordRequest) (*pb.ForgotPasswordResponse, error) {
	const safeReply = "If your email is registered in our system, you will receive a password reset link."

	user, err := h.querier.GetUserByEmail(ctx, req.Email)
	if err != nil {
		// sql.ErrNoRows or any other DB error — swallow and return safe reply.
		return &pb.ForgotPasswordResponse{Message: safeReply}, nil
	}

	// Only generate a token for active accounts; inactive accounts are silently
	// skipped (same safe reply — no information leaked).
	if user.IsActive {
		token, err := auth.GenerateResetToken(req.Email, h.activationSecret)
		if err != nil {
			log.Printf("[forgot-password] failed to generate reset token for %s: %v", req.Email, err)
		} else if err := h.publisher.Publish(utils.EmailEvent{
			Type:  "RESET",
			Email: req.Email,
			Token: token,
		}); err != nil {
			log.Printf("[forgot-password] failed to publish reset event for %s: %v", req.Email, err)
		}
	}

	return &pb.ForgotPasswordResponse{Message: safeReply}, nil
}

// ResetPassword consumes a password-reset JWT and writes the new password.
//
// Flow (no auth header required — the reset token IS the credential):
//  1. Validate new password length (min 8 characters).
//  2. Verify the reset JWT — Unauthenticated if invalid or expired.
//  3. Hash the new password with bcrypt.
//  4. Persist via UpdateUserPassword.
//
// Mapped to: POST /auth/reset-password
func (h *UserHandler) ResetPassword(ctx context.Context, req *pb.ResetPasswordRequest) (*pb.ResetPasswordResponse, error) {
	// ── 1. Password validation ────────────────────────────────────────────────
	if len(req.NewPassword) < 8 {
		return nil, status.Errorf(codes.InvalidArgument, "password must be at least 8 characters")
	}

	// ── 2. Verify reset token ─────────────────────────────────────────────────
	email, err := auth.VerifyResetToken(req.Token, h.activationSecret)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid or expired reset token")
	}

	// ── 3. Hash password ──────────────────────────────────────────────────────
	hashed, err := utils.HashPassword(req.NewPassword)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to hash password")
	}

	// ── 4. Persist ────────────────────────────────────────────────────────────
	if err := h.querier.UpdateUserPassword(ctx, db.UpdateUserPasswordParams{
		PasswordHash: hashed,
		Email:        email,
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to update password")
	}

	// ── 5. Notify user of the password change ─────────────────────────────────
	// Fire-and-forget: the password is already committed, so a messaging failure
	// must not surface as a gRPC error to the caller.
	if err := h.publisher.Publish(utils.EmailEvent{
		Type:  "PASSWORD_RESET_SUCCESS",
		Email: email,
		Token: "",
	}); err != nil {
		log.Printf("[reset-password] failed to publish confirmation event for %s: %v", email, err)
	}

	return &pb.ResetPasswordResponse{Message: "Password reset successfully."}, nil
}

// GetMyProfile vraća profil trenutno prijavljenog korisnika.
//
// Mapped to: GET /user/me
func (h *UserHandler) GetMyProfile(ctx context.Context, _ *pb.GetMyProfileRequest) (*pb.GetMyProfileResponse, error) {
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "neautorizovan pristup")
	}

	userID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "neispravan korisnički ID u tokenu")
	}

	user, err := h.querier.GetUserByID(ctx, userID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "korisnik nije pronađen")
	}

	return &pb.GetMyProfileResponse{
		Id:        user.ID,
		Email:     user.Email,
		FirstName: user.FirstName,
		LastName:  user.LastName,
	}, nil
}
