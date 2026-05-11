//go:build unit

package provider

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/stretchr/testify/require"
)

func TestOfflineProviderCreatesPendingManualPayment(t *testing.T) {
	t.Parallel()

	prov, err := NewOffline("offline-1", map[string]string{"unused": "value"})
	require.NoError(t, err)

	require.Equal(t, payment.TypeOffline, prov.ProviderKey())
	require.Equal(t, "Offline", prov.Name())
	require.Equal(t, []payment.PaymentType{payment.TypeOffline}, prov.SupportedTypes())

	resp, err := prov.CreatePayment(context.Background(), payment.CreatePaymentRequest{
		OrderID:     "sub2_offline_1",
		Amount:      "88.00",
		PaymentType: payment.TypeOffline,
	})
	require.NoError(t, err)
	require.Equal(t, payment.CreatePaymentResultOfflinePending, resp.ResultType)
	require.Equal(t, payment.DefaultPaymentCurrency, resp.Currency)
	require.Empty(t, resp.TradeNo)
	require.Empty(t, resp.PayURL)
	require.Empty(t, resp.QRCode)

	queryResp, err := prov.QueryOrder(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, payment.ProviderStatusPending, queryResp.Status)

	notification, err := prov.VerifyNotification(context.Background(), "", nil)
	require.NoError(t, err)
	require.Nil(t, notification)

	_, err = prov.Refund(context.Background(), payment.RefundRequest{})
	require.ErrorContains(t, err, "offline payments do not support gateway refunds")
}
