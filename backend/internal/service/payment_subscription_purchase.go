package service

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/user"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// PurchaseSubscriptionWithBalanceResult is returned after a subscription plan
// is paid from the user's stored balance.
type PurchaseSubscriptionWithBalanceResult struct {
	Subscription  *UserSubscription
	Created       bool
	Balance       float64
	ChargedAmount float64
	PlanID        int64
}

// PurchaseSubscriptionWithBalance buys a subscription plan with account balance.
// Balance deduction, subscription assignment, and ledger creation are committed atomically.
func (s *PaymentService) PurchaseSubscriptionWithBalance(ctx context.Context, userID, planID int64) (*PurchaseSubscriptionWithBalanceResult, error) {
	if s == nil || s.entClient == nil || s.configService == nil || s.groupRepo == nil || s.userRepo == nil || s.subscriptionSvc == nil || s.redeemService == nil {
		return nil, infraerrors.InternalServer("PAYMENT_SERVICE_UNAVAILABLE", "payment service is unavailable")
	}
	if userID <= 0 || planID <= 0 {
		return nil, infraerrors.BadRequest("INVALID_INPUT", "invalid subscription purchase input")
	}
	cfg, err := s.configService.GetPaymentConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("get payment config: %w", err)
	}
	if !cfg.Enabled {
		return nil, infraerrors.Forbidden("PAYMENT_DISABLED", "payment system is disabled")
	}

	plan, err := s.configService.GetPlan(ctx, planID)
	if err != nil || plan == nil || !plan.ForSale {
		return nil, infraerrors.NotFound("PLAN_NOT_AVAILABLE", "plan not found or no longer available")
	}
	if math.IsNaN(plan.Price) || math.IsInf(plan.Price, 0) || plan.Price <= 0 {
		return nil, infraerrors.BadRequest("INVALID_AMOUNT", "plan price must be positive")
	}

	group, err := s.groupRepo.GetByID(ctx, plan.GroupID)
	if err != nil || group == nil || group.Status != StatusActive {
		return nil, infraerrors.NotFound("GROUP_NOT_FOUND", "subscription group is no longer available")
	}
	if !group.IsSubscriptionType() {
		return nil, infraerrors.BadRequest("GROUP_TYPE_MISMATCH", "group is not a subscription type")
	}

	u, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	if u.Status != StatusActive {
		return nil, infraerrors.Forbidden("USER_INACTIVE", "user account is disabled")
	}

	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	txCtx := dbent.NewTxContext(ctx, tx)
	balance, err := s.deductSubscriptionBalance(txCtx, tx.Client(), userID, plan.Price)
	if err != nil {
		return nil, err
	}

	validityDays := psComputeValidityDays(plan.ValidityDays, plan.ValidityUnit)
	sub, extended, err := s.subscriptionSvc.AssignOrExtendSubscription(txCtx, &AssignSubscriptionInput{
		UserID:       userID,
		GroupID:      plan.GroupID,
		ValidityDays: validityDays,
		AssignedBy:   0,
		Notes:        subscriptionPurchaseNote(plan.Name),
	})
	if err != nil {
		return nil, fmt.Errorf("assign subscription: %w", err)
	}

	if err := s.createSubscriptionPurchaseLedger(txCtx, userID, plan.GroupID, validityDays, plan.Price, plan.Name); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit subscription purchase: %w", err)
	}

	s.invalidateSubscriptionPurchaseCaches(ctx, userID, plan.GroupID)

	return &PurchaseSubscriptionWithBalanceResult{
		Subscription:  sub,
		Created:       !extended,
		Balance:       balance,
		ChargedAmount: plan.Price,
		PlanID:        planID,
	}, nil
}

func (s *PaymentService) deductSubscriptionBalance(ctx context.Context, client *dbent.Client, userID int64, amount float64) (float64, error) {
	affected, err := client.User.Update().
		Where(user.IDEQ(userID), user.StatusEQ(StatusActive), user.BalanceGTE(amount)).
		AddBalance(-amount).
		Save(ctx)
	if err != nil {
		return 0, fmt.Errorf("deduct balance: %w", err)
	}
	if affected == 0 {
		current, getErr := client.User.Get(ctx, userID)
		if getErr == nil && current.Status != StatusActive {
			return 0, infraerrors.Forbidden("USER_INACTIVE", "user account is disabled")
		}
		return 0, ErrInsufficientBalance
	}

	current, err := client.User.Get(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("get updated balance: %w", err)
	}
	return current.Balance, nil
}

func (s *PaymentService) createSubscriptionPurchaseLedger(ctx context.Context, userID, groupID int64, validityDays int, amount float64, planName string) error {
	now := time.Now()
	usedBy := userID
	groupIDPtr := groupID
	code := &RedeemCode{
		Code:         "SUB-" + generateRandomString(28),
		Type:         RedeemTypeSubscriptionPurchase,
		Value:        -amount,
		Status:       StatusUsed,
		UsedBy:       &usedBy,
		UsedAt:       &now,
		GroupID:      &groupIDPtr,
		ValidityDays: validityDays,
		Notes:        subscriptionPurchaseNote(planName),
	}
	if err := s.redeemService.CreateCode(ctx, code); err != nil {
		return fmt.Errorf("create subscription purchase ledger: %w", err)
	}
	return nil
}

func subscriptionPurchaseNote(planName string) string {
	name := strings.TrimSpace(planName)
	if name == "" {
		return "subscription purchase"
	}
	return fmt.Sprintf("subscription purchase: %s", name)
}

func (s *PaymentService) invalidateSubscriptionPurchaseCaches(ctx context.Context, userID, groupID int64) {
	groupIDPtr := groupID
	if s.subscriptionSvc != nil {
		s.subscriptionSvc.InvalidateSubCache(userID, groupID)
	}
	if s.redeemService != nil {
		s.redeemService.invalidateRedeemCaches(ctx, userID, &RedeemCode{
			Type:    RedeemTypeSubscriptionPurchase,
			GroupID: &groupIDPtr,
		})
	}
}
