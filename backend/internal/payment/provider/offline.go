package provider

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/payment"
)

// Offline represents manual, administrator-confirmed collection outside an online gateway.
type Offline struct {
	instanceID string
	config     map[string]string
}

func NewOffline(instanceID string, config map[string]string) (*Offline, error) {
	cfg := cloneStringMap(config)
	return &Offline{instanceID: instanceID, config: cfg}, nil
}

func (o *Offline) Name() string { return "Offline" }

func (o *Offline) ProviderKey() string { return payment.TypeOffline }

func (o *Offline) SupportedTypes() []payment.PaymentType {
	return []payment.PaymentType{payment.TypeOffline}
}

func (o *Offline) CreatePayment(context.Context, payment.CreatePaymentRequest) (*payment.CreatePaymentResponse, error) {
	return &payment.CreatePaymentResponse{
		Currency:   payment.DefaultPaymentCurrency,
		ResultType: payment.CreatePaymentResultOfflinePending,
	}, nil
}

func (o *Offline) QueryOrder(context.Context, string) (*payment.QueryOrderResponse, error) {
	return &payment.QueryOrderResponse{Status: payment.ProviderStatusPending}, nil
}

func (o *Offline) VerifyNotification(context.Context, string, map[string]string) (*payment.PaymentNotification, error) {
	return nil, nil
}

func (o *Offline) Refund(context.Context, payment.RefundRequest) (*payment.RefundResponse, error) {
	return nil, fmt.Errorf("offline payments do not support gateway refunds")
}
