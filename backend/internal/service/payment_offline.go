package service

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	offlinePaymentMaxReferenceLen = 128
	offlinePaymentMaxNoteLen      = 500
)

type ConfirmOfflinePaymentRequest struct {
	Amount    float64
	Reference string
	Note      string
}

func (s *PaymentService) AdminConfirmOfflinePayment(ctx context.Context, orderID int64, req ConfirmOfflinePaymentRequest) error {
	o, err := s.entClient.PaymentOrder.Get(ctx, orderID)
	if err != nil {
		return infraerrors.NotFound("NOT_FOUND", "order not found")
	}
	if payment.GetBasePaymentType(o.PaymentType) != payment.TypeOffline {
		return infraerrors.BadRequest("OFFLINE_PAYMENT_ONLY", "only offline payment orders can be confirmed")
	}
	if o.Status != OrderStatusPending {
		return infraerrors.BadRequest("INVALID_STATUS", "only pending offline orders can be confirmed")
	}
	if o.PaidAt != nil {
		return infraerrors.Conflict("OFFLINE_PAYMENT_ALREADY_PAID", "offline payment order has already been paid")
	}

	paidAmount := req.Amount
	if paidAmount <= 0 {
		paidAmount = o.PayAmount
	}
	if !isValidProviderAmount(paidAmount) {
		return infraerrors.BadRequest("INVALID_AMOUNT", "amount must be a positive number")
	}
	if math.Abs(paidAmount-o.PayAmount) > paymentAmountToleranceForCurrency(PaymentOrderCurrency(o)) {
		return infraerrors.BadRequest("PAYMENT_AMOUNT_MISMATCH", "offline paid amount does not match order amount").
			WithMetadata(map[string]string{
				"expected": strconv.FormatFloat(o.PayAmount, 'f', -1, 64),
				"paid":     strconv.FormatFloat(paidAmount, 'f', -1, 64),
			})
	}

	reference := strings.TrimSpace(req.Reference)
	if len(reference) > offlinePaymentMaxReferenceLen {
		return infraerrors.BadRequest("INVALID_INPUT", fmt.Sprintf("reference must be at most %d characters", offlinePaymentMaxReferenceLen))
	}
	note := strings.TrimSpace(req.Note)
	if len(note) > offlinePaymentMaxNoteLen {
		return infraerrors.BadRequest("INVALID_INPUT", fmt.Sprintf("note must be at most %d characters", offlinePaymentMaxNoteLen))
	}

	if err := s.toPaid(ctx, o, reference, paidAmount, payment.TypeOffline); err != nil {
		return err
	}
	s.writeAuditLog(ctx, o.ID, "OFFLINE_PAYMENT_CONFIRMED", "admin", map[string]any{
		"reference":  reference,
		"note":       note,
		"paidAmount": paidAmount,
	})
	return nil
}
