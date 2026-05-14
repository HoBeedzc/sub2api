//go:build unit

package service_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/ent/redeemcode"
	"github.com/Wei-Shaw/sub2api/ent/usersubscription"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "modernc.org/sqlite"
)

func TestPurchaseSubscriptionWithBalance_CreatesSubscriptionAndLedger(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newPaymentSubscriptionPurchaseTestClient(t)
	userEnt, groupEnt, planEnt := seedSubscriptionPurchaseData(t, ctx, client, 100, 30)
	paymentSvc := newPaymentSubscriptionPurchaseService(t, client, userEnt.ID, groupEnt.ID, 100)

	result, err := paymentSvc.PurchaseSubscriptionWithBalance(ctx, userEnt.ID, planEnt.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Created)
	require.Equal(t, planEnt.ID, result.PlanID)
	require.InDelta(t, 30, result.ChargedAmount, 0.000001)
	require.InDelta(t, 70, result.Balance, 0.000001)

	userAfter, err := client.User.Get(ctx, userEnt.ID)
	require.NoError(t, err)
	require.InDelta(t, 70, userAfter.Balance, 0.000001)

	subCount, err := client.UserSubscription.Query().
		Where(
			usersubscription.UserIDEQ(userEnt.ID),
			usersubscription.GroupIDEQ(groupEnt.ID),
		).
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, subCount)

	ledger, err := client.RedeemCode.Query().
		Where(
			redeemcode.UsedByEQ(userEnt.ID),
			redeemcode.TypeEQ(service.RedeemTypeSubscriptionPurchase),
		).
		Only(ctx)
	require.NoError(t, err)
	require.Equal(t, service.StatusUsed, ledger.Status)
	require.NotNil(t, ledger.GroupID)
	require.Equal(t, groupEnt.ID, *ledger.GroupID)
	require.NotNil(t, ledger.UsedBy)
	require.Equal(t, userEnt.ID, *ledger.UsedBy)
	require.NotNil(t, ledger.UsedAt)
	require.Equal(t, 30, ledger.ValidityDays)
	require.InDelta(t, -30, ledger.Value, 0.000001)
	require.True(t, strings.HasPrefix(ledger.Code, "SUB-"))
	require.Len(t, ledger.Code, 32)
	require.NotNil(t, ledger.Notes)
	require.Contains(t, *ledger.Notes, "Starter")
}

func TestPurchaseSubscriptionWithBalance_InsufficientBalanceRollsBack(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newPaymentSubscriptionPurchaseTestClient(t)
	userEnt, groupEnt, planEnt := seedSubscriptionPurchaseData(t, ctx, client, 10, 30)
	paymentSvc := newPaymentSubscriptionPurchaseService(t, client, userEnt.ID, groupEnt.ID, 10)

	result, err := paymentSvc.PurchaseSubscriptionWithBalance(ctx, userEnt.ID, planEnt.ID)

	require.Nil(t, result)
	require.ErrorIs(t, err, service.ErrInsufficientBalance)

	userAfter, err := client.User.Get(ctx, userEnt.ID)
	require.NoError(t, err)
	require.InDelta(t, 10, userAfter.Balance, 0.000001)

	subCount, err := client.UserSubscription.Query().
		Where(
			usersubscription.UserIDEQ(userEnt.ID),
			usersubscription.GroupIDEQ(groupEnt.ID),
		).
		Count(ctx)
	require.NoError(t, err)
	require.Zero(t, subCount)

	ledgerCount, err := client.RedeemCode.Query().
		Where(redeemcode.TypeEQ(service.RedeemTypeSubscriptionPurchase)).
		Count(ctx)
	require.NoError(t, err)
	require.Zero(t, ledgerCount)
}

func newPaymentSubscriptionPurchaseTestClient(t *testing.T) *dbent.Client {
	t.Helper()

	dbName := fmt.Sprintf(
		"file:%s?mode=memory&cache=shared&_fk=1",
		strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()),
	)
	db, err := sql.Open("sqlite", dbName)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)

	drv := entsql.OpenDB(dialect.SQLite, db)
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(drv)))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func seedSubscriptionPurchaseData(
	t *testing.T,
	ctx context.Context,
	client *dbent.Client,
	userBalance float64,
	planPrice float64,
) (*dbent.User, *dbent.Group, *dbent.SubscriptionPlan) {
	t.Helper()

	userEnt, err := client.User.Create().
		SetEmail(fmt.Sprintf("buyer-%s@example.com", strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()))).
		SetPasswordHash("hash").
		SetStatus(service.StatusActive).
		SetBalance(userBalance).
		Save(ctx)
	require.NoError(t, err)

	groupEnt, err := client.Group.Create().
		SetName(fmt.Sprintf("group-%s", strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()))).
		SetStatus(service.StatusActive).
		SetPlatform(service.PlatformOpenAI).
		SetRateMultiplier(1).
		SetSubscriptionType(service.SubscriptionTypeSubscription).
		Save(ctx)
	require.NoError(t, err)

	planEnt, err := client.SubscriptionPlan.Create().
		SetGroupID(groupEnt.ID).
		SetName("Starter").
		SetDescription("Starter plan").
		SetPrice(planPrice).
		SetValidityDays(30).
		SetValidityUnit("day").
		SetFeatures("").
		SetForSale(true).
		SetSortOrder(1).
		Save(ctx)
	require.NoError(t, err)

	return userEnt, groupEnt, planEnt
}

func newPaymentSubscriptionPurchaseService(
	t *testing.T,
	client *dbent.Client,
	userID int64,
	groupID int64,
	userBalance float64,
) *service.PaymentService {
	t.Helper()

	userRepo := &subscriptionPurchaseUserRepoStub{users: map[int64]*service.User{
		userID: {
			ID:      userID,
			Status:  service.StatusActive,
			Balance: userBalance,
		},
	}}
	groupRepo := &subscriptionPurchaseGroupRepoStub{groups: map[int64]*service.Group{
		groupID: {
			ID:               groupID,
			Name:             "OpenAI subscription",
			Status:           service.StatusActive,
			Platform:         service.PlatformOpenAI,
			RateMultiplier:   1,
			SubscriptionType: service.SubscriptionTypeSubscription,
		},
	}}
	configSvc := service.NewPaymentConfigService(
		client,
		&subscriptionPurchaseSettingRepoStub{values: map[string]string{
			service.SettingPaymentEnabled: "true",
		}},
		nil,
	)
	userSubRepo := repository.NewUserSubscriptionRepository(client)
	subscriptionSvc := service.NewSubscriptionService(groupRepo, userSubRepo, nil, client, nil)
	redeemSvc := service.NewRedeemService(
		repository.NewRedeemCodeRepository(client),
		userRepo,
		subscriptionSvc,
		nil,
		nil,
		client,
		nil,
		nil,
	)

	return service.NewPaymentService(
		client,
		payment.NewRegistry(),
		nil,
		redeemSvc,
		subscriptionSvc,
		configSvc,
		userRepo,
		groupRepo,
		nil,
	)
}

type subscriptionPurchaseSettingRepoStub struct {
	values map[string]string
}

func (s *subscriptionPurchaseSettingRepoStub) Get(context.Context, string) (*service.Setting, error) {
	return nil, nil
}

func (s *subscriptionPurchaseSettingRepoStub) GetValue(_ context.Context, key string) (string, error) {
	return s.values[key], nil
}

func (s *subscriptionPurchaseSettingRepoStub) Set(context.Context, string, string) error { return nil }

func (s *subscriptionPurchaseSettingRepoStub) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		out[key] = s.values[key]
	}
	return out, nil
}

func (s *subscriptionPurchaseSettingRepoStub) SetMultiple(_ context.Context, values map[string]string) error {
	if s.values == nil {
		s.values = map[string]string{}
	}
	for key, value := range values {
		s.values[key] = value
	}
	return nil
}

func (s *subscriptionPurchaseSettingRepoStub) GetAll(context.Context) (map[string]string, error) {
	return s.values, nil
}

func (s *subscriptionPurchaseSettingRepoStub) Delete(context.Context, string) error { return nil }

type subscriptionPurchaseUserRepoStub struct {
	service.UserRepository
	users map[int64]*service.User
}

func (r *subscriptionPurchaseUserRepoStub) GetByID(_ context.Context, id int64) (*service.User, error) {
	u := r.users[id]
	if u == nil {
		return nil, service.ErrUserNotFound
	}
	copied := *u
	return &copied, nil
}

type subscriptionPurchaseGroupRepoStub struct {
	service.GroupRepository
	groups map[int64]*service.Group
}

func (r *subscriptionPurchaseGroupRepoStub) GetByID(_ context.Context, id int64) (*service.Group, error) {
	g := r.groups[id]
	if g == nil {
		return nil, service.ErrGroupNotFound
	}
	copied := *g
	return &copied, nil
}
