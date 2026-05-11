//go:build unit

package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestAdminConfirmOfflinePaymentMarksOrderPaidAndFulfills(t *testing.T) {
	ctx := context.Background()
	client := newPaymentOrderLifecycleTestClient(t)
	user, order := createOfflineConfirmPaymentOrder(t, ctx, client, payment.TypeOffline, OrderStatusPending, 88)

	userRepo := &mockUserRepo{
		getByIDUser: &User{
			ID:       user.ID,
			Email:    user.Email,
			Username: user.Username,
			Balance:  0,
		},
	}
	userRepo.updateBalanceFn = func(ctx context.Context, id int64, amount float64) error {
		require.Equal(t, user.ID, id)
		userRepo.getByIDUser.Balance += amount
		return nil
	}
	redeemRepo := &paymentOrderLifecycleRedeemRepo{
		codesByCode: map[string]*RedeemCode{
			order.RechargeCode: {
				ID:     1,
				Code:   order.RechargeCode,
				Type:   RedeemTypeBalance,
				Value:  order.Amount,
				Status: StatusUnused,
			},
		},
	}
	redeemService := NewRedeemService(redeemRepo, userRepo, nil, nil, nil, client, nil, nil)
	svc := &PaymentService{
		entClient:     client,
		redeemService: redeemService,
		userRepo:      userRepo,
	}

	err := svc.AdminConfirmOfflinePayment(ctx, order.ID, ConfirmOfflinePaymentRequest{
		Amount:    88,
		Reference: "cash-001",
		Note:      "front desk",
	})
	require.NoError(t, err)

	reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusCompleted, reloaded.Status)
	require.Equal(t, "cash-001", reloaded.PaymentTradeNo)
	require.NotNil(t, reloaded.PaidAt)
	require.NotNil(t, reloaded.CompletedAt)
	require.Equal(t, 88.0, userRepo.getByIDUser.Balance)

	logs, err := svc.GetOrderAuditLogs(ctx, order.ID)
	require.NoError(t, err)
	actions := make(map[string]string, len(logs))
	for _, log := range logs {
		actions[log.Action] = log.Detail
	}
	require.Contains(t, actions, "ORDER_PAID")
	require.Contains(t, actions, "RECHARGE_SUCCESS")
	require.Contains(t, actions, "OFFLINE_PAYMENT_CONFIRMED")
	require.Contains(t, actions["OFFLINE_PAYMENT_CONFIRMED"], "cash-001")
}

func TestAdminConfirmOfflinePaymentRejectsUnsupportedOrders(t *testing.T) {
	ctx := context.Background()
	client := newPaymentOrderLifecycleTestClient(t)
	_, onlineOrder := createOfflineConfirmPaymentOrder(t, ctx, client, payment.TypeAlipay, OrderStatusPending, 88)

	svc := &PaymentService{entClient: client}
	err := svc.AdminConfirmOfflinePayment(ctx, onlineOrder.ID, ConfirmOfflinePaymentRequest{Amount: 88})
	require.Error(t, err)
	require.Equal(t, "OFFLINE_PAYMENT_ONLY", infraerrors.Reason(err))
}

func TestAdminConfirmOfflinePaymentRejectsAmountMismatch(t *testing.T) {
	ctx := context.Background()
	client := newPaymentOrderLifecycleTestClient(t)
	_, order := createOfflineConfirmPaymentOrder(t, ctx, client, payment.TypeOffline, OrderStatusPending, 88)

	svc := &PaymentService{entClient: client}
	err := svc.AdminConfirmOfflinePayment(ctx, order.ID, ConfirmOfflinePaymentRequest{Amount: 87.98})
	require.Error(t, err)
	require.Equal(t, "PAYMENT_AMOUNT_MISMATCH", infraerrors.Reason(err))

	reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPending, reloaded.Status)
	require.Empty(t, reloaded.PaymentTradeNo)
}

func createOfflineConfirmPaymentOrder(
	t *testing.T,
	ctx context.Context,
	client *dbent.Client,
	paymentType string,
	status string,
	payAmount float64,
) (*dbent.User, *dbent.PaymentOrder) {
	t.Helper()

	suffix := time.Now().UnixNano()
	user, err := client.User.Create().
		SetEmail(fmt.Sprintf("offline-%d@example.com", suffix)).
		SetPasswordHash("hash").
		SetUsername("offline-user").
		Save(ctx)
	require.NoError(t, err)

	orderCreate := client.PaymentOrder.Create().
		SetUserID(user.ID).
		SetUserEmail(user.Email).
		SetUserName(user.Username).
		SetAmount(payAmount).
		SetPayAmount(payAmount).
		SetFeeRate(0).
		SetRechargeCode(fmt.Sprintf("OFFLINE-%d", suffix)).
		SetOutTradeNo(fmt.Sprintf("sub2_offline_%d", suffix)).
		SetPaymentType(paymentType).
		SetPaymentTradeNo("").
		SetOrderType(payment.OrderTypeBalance).
		SetStatus(status).
		SetExpiresAt(time.Now().Add(time.Hour)).
		SetClientIP("127.0.0.1").
		SetSrcHost("api.example.com")
	if paymentType == payment.TypeOffline {
		orderCreate.SetProviderKey(payment.TypeOffline).
			SetProviderSnapshot(map[string]any{
				"schema_version": 2,
				"provider_key":   payment.TypeOffline,
				"currency":       payment.DefaultPaymentCurrency,
			})
	}
	order, err := orderCreate.Save(ctx)
	require.NoError(t, err)
	return user, order
}
