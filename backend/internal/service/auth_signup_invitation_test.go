//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type signupInvitationAffiliateRepoStub struct {
	inviter     *AffiliateSummary
	self        *AffiliateSummary
	bindUserID  int64
	bindInviter int64
	rejectBind  bool
}

func (r *signupInvitationAffiliateRepoStub) EnsureUserAffiliate(_ context.Context, userID int64) (*AffiliateSummary, error) {
	if r.self == nil {
		r.self = &AffiliateSummary{UserID: userID, AffCode: "SELF-CODE"}
	}
	copy := *r.self
	copy.UserID = userID
	return &copy, nil
}

func (r *signupInvitationAffiliateRepoStub) GetAffiliateByCode(_ context.Context, code string) (*AffiliateSummary, error) {
	if r.inviter == nil || r.inviter.AffCode != code {
		return nil, ErrAffiliateProfileNotFound
	}
	copy := *r.inviter
	return &copy, nil
}

func (r *signupInvitationAffiliateRepoStub) BindInviter(_ context.Context, userID, inviterID int64) (bool, error) {
	if r.rejectBind {
		return false, nil
	}
	r.bindUserID = userID
	r.bindInviter = inviterID
	return true, nil
}

func (r *signupInvitationAffiliateRepoStub) AccrueQuota(context.Context, int64, int64, float64, int, *int64) (bool, error) {
	panic("unexpected AccrueQuota call")
}
func (r *signupInvitationAffiliateRepoStub) GetAccruedRebateFromInvitee(context.Context, int64, int64) (float64, error) {
	panic("unexpected GetAccruedRebateFromInvitee call")
}
func (r *signupInvitationAffiliateRepoStub) ThawFrozenQuota(context.Context, int64) (float64, error) {
	panic("unexpected ThawFrozenQuota call")
}
func (r *signupInvitationAffiliateRepoStub) TransferQuotaToBalance(context.Context, int64) (float64, float64, error) {
	panic("unexpected TransferQuotaToBalance call")
}
func (r *signupInvitationAffiliateRepoStub) ListInvitees(context.Context, int64, int) ([]AffiliateInvitee, error) {
	panic("unexpected ListInvitees call")
}
func (r *signupInvitationAffiliateRepoStub) UpdateUserAffCode(context.Context, int64, string) error {
	panic("unexpected UpdateUserAffCode call")
}
func (r *signupInvitationAffiliateRepoStub) ResetUserAffCode(context.Context, int64) (string, error) {
	panic("unexpected ResetUserAffCode call")
}
func (r *signupInvitationAffiliateRepoStub) SetUserRebateRate(context.Context, int64, *float64) error {
	panic("unexpected SetUserRebateRate call")
}
func (r *signupInvitationAffiliateRepoStub) BatchSetUserRebateRate(context.Context, []int64, *float64) error {
	panic("unexpected BatchSetUserRebateRate call")
}
func (r *signupInvitationAffiliateRepoStub) ListUsersWithCustomSettings(context.Context, AffiliateAdminFilter) ([]AffiliateAdminEntry, int64, error) {
	panic("unexpected ListUsersWithCustomSettings call")
}
func (r *signupInvitationAffiliateRepoStub) ListAffiliateInviteRecords(context.Context, AffiliateRecordFilter) ([]AffiliateInviteRecord, int64, error) {
	panic("unexpected ListAffiliateInviteRecords call")
}
func (r *signupInvitationAffiliateRepoStub) ListAffiliateRebateRecords(context.Context, AffiliateRecordFilter) ([]AffiliateRebateRecord, int64, error) {
	panic("unexpected ListAffiliateRebateRecords call")
}
func (r *signupInvitationAffiliateRepoStub) ListAffiliateTransferRecords(context.Context, AffiliateRecordFilter) ([]AffiliateTransferRecord, int64, error) {
	panic("unexpected ListAffiliateTransferRecords call")
}
func (r *signupInvitationAffiliateRepoStub) GetAffiliateUserOverview(context.Context, int64) (*AffiliateUserOverview, error) {
	panic("unexpected GetAffiliateUserOverview call")
}

func newSignupInvitationAuthService(userRepo *userRepoStub, redeemRepo RedeemCodeRepository, affiliateRepo AffiliateRepository) *AuthService {
	cfg := &config.Config{
		JWT:     config.JWTConfig{Secret: "test-secret", ExpireHour: 1},
		Default: config.DefaultConfig{UserBalance: 0, UserConcurrency: 1},
	}
	settings := map[string]string{
		SettingKeyRegistrationEnabled:   "true",
		SettingKeyInvitationCodeEnabled: "true",
		SettingKeyAffiliateEnabled:      "false",
	}
	settingService := NewSettingService(&settingRepoStub{values: settings}, cfg)
	var affiliateService *AffiliateService
	if affiliateRepo != nil {
		affiliateService = NewAffiliateService(affiliateRepo, settingService, nil, nil)
	}
	return NewAuthService(nil, userRepo, redeemRepo, nil, cfg, settingService, nil, nil, nil, nil, nil, affiliateService, nil)
}

func TestRegisterWithVerificationAcceptsExistingUserAffiliateCode(t *testing.T) {
	tests := []struct {
		name           string
		invitationCode string
		affiliateCode  string
	}{
		{name: "invitation field", invitationCode: "FRIEND123"},
		{name: "affiliate field", affiliateCode: "FRIEND123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			userRepo := &userRepoStub{nextID: 42}
			affiliateRepo := &signupInvitationAffiliateRepoStub{
				inviter: &AffiliateSummary{UserID: 7, AffCode: "FRIEND123", CreatedAt: time.Now()},
			}
			svc := newSignupInvitationAuthService(userRepo, nil, affiliateRepo)

			_, user, err := svc.RegisterWithVerification(
				context.Background(),
				"friend@example.com",
				"secret-123",
				"",
				"",
				tt.invitationCode,
				tt.affiliateCode,
			)

			require.NoError(t, err)
			require.Equal(t, int64(42), user.ID)
			require.Equal(t, int64(42), affiliateRepo.bindUserID)
			require.Equal(t, int64(7), affiliateRepo.bindInviter)
		})
	}
}

func TestRegisterWithVerificationRejectsUnknownAffiliateCode(t *testing.T) {
	userRepo := &userRepoStub{nextID: 42}
	svc := newSignupInvitationAuthService(userRepo, nil, &signupInvitationAffiliateRepoStub{})

	_, user, err := svc.RegisterWithVerification(
		context.Background(),
		"unknown@example.com",
		"secret-123",
		"",
		"",
		"",
		"UNKNOWN1",
	)

	require.ErrorIs(t, err, ErrInvitationCodeInvalid)
	require.Nil(t, user)
	require.Empty(t, userRepo.created)
}

func TestRegisterWithVerificationStillAcceptsOneTimeInvitation(t *testing.T) {
	userRepo := &userRepoStub{nextID: 42}
	redeemRepo := &signupInvitationRedeemRepoStub{
		code: &RedeemCode{ID: 9, Code: "ONETIME1", Type: RedeemTypeInvitation, Status: StatusUnused},
	}
	svc := newSignupInvitationAuthService(userRepo, redeemRepo, nil)

	_, user, err := svc.RegisterWithVerification(
		context.Background(),
		"onetime@example.com",
		"secret-123",
		"",
		"",
		"ONETIME1",
		"",
	)

	require.NoError(t, err)
	require.Equal(t, int64(42), user.ID)
	require.Equal(t, int64(42), redeemRepo.usedBy)
}

func TestRegisterWithVerificationRollsBackWhenAffiliateBindingFails(t *testing.T) {
	userRepo := &userRepoStub{nextID: 42}
	affiliateRepo := &signupInvitationAffiliateRepoStub{
		inviter:    &AffiliateSummary{UserID: 7, AffCode: "FRIEND123", CreatedAt: time.Now()},
		rejectBind: true,
	}
	svc := newSignupInvitationAuthService(userRepo, nil, affiliateRepo)

	_, user, err := svc.RegisterWithVerification(
		context.Background(),
		"rejected@example.com",
		"secret-123",
		"",
		"",
		"FRIEND123",
		"",
	)

	require.ErrorIs(t, err, ErrInvitationCodeInvalid)
	require.Nil(t, user)
	require.Equal(t, []int64{42}, userRepo.deletedIDs)
}

func TestRegisterWithVerificationRollsBackWhenInvitationWasConcurrentlyUsed(t *testing.T) {
	userRepo := &userRepoStub{nextID: 42}
	redeemRepo := &signupInvitationRedeemRepoStub{
		code:   &RedeemCode{ID: 9, Code: "ONETIME1", Type: RedeemTypeInvitation, Status: StatusUnused},
		useErr: ErrRedeemCodeUsed,
	}
	svc := newSignupInvitationAuthService(userRepo, redeemRepo, nil)

	_, user, err := svc.RegisterWithVerification(
		context.Background(),
		"raced@example.com",
		"secret-123",
		"",
		"",
		"ONETIME1",
		"",
	)

	require.ErrorIs(t, err, ErrInvitationCodeInvalid)
	require.Nil(t, user)
	require.Equal(t, []int64{42}, userRepo.deletedIDs)
}

type signupInvitationRedeemRepoStub struct {
	code   *RedeemCode
	usedBy int64
	useErr error
}

func (r *signupInvitationRedeemRepoStub) Create(context.Context, *RedeemCode) error {
	panic("unexpected Create call")
}
func (r *signupInvitationRedeemRepoStub) CreateBatch(context.Context, []RedeemCode) error {
	panic("unexpected CreateBatch call")
}
func (r *signupInvitationRedeemRepoStub) GetByID(context.Context, int64) (*RedeemCode, error) {
	panic("unexpected GetByID call")
}
func (r *signupInvitationRedeemRepoStub) GetByCode(_ context.Context, code string) (*RedeemCode, error) {
	if r.code == nil || r.code.Code != code {
		return nil, ErrRedeemCodeNotFound
	}
	copy := *r.code
	return &copy, nil
}
func (r *signupInvitationRedeemRepoStub) Update(context.Context, *RedeemCode) error {
	panic("unexpected Update call")
}
func (r *signupInvitationRedeemRepoStub) BatchUpdate(context.Context, []int64, RedeemCodeBatchUpdateFields) (int64, error) {
	panic("unexpected BatchUpdate call")
}
func (r *signupInvitationRedeemRepoStub) Delete(context.Context, int64) error {
	panic("unexpected Delete call")
}
func (r *signupInvitationRedeemRepoStub) Use(_ context.Context, _ int64, userID int64) error {
	if r.useErr != nil {
		return r.useErr
	}
	r.usedBy = userID
	return nil
}
func (r *signupInvitationRedeemRepoStub) List(context.Context, pagination.PaginationParams) ([]RedeemCode, *pagination.PaginationResult, error) {
	panic("unexpected List call")
}
func (r *signupInvitationRedeemRepoStub) ListWithFilters(context.Context, pagination.PaginationParams, string, string, string) ([]RedeemCode, *pagination.PaginationResult, error) {
	panic("unexpected ListWithFilters call")
}
func (r *signupInvitationRedeemRepoStub) ListByUser(context.Context, int64, int) ([]RedeemCode, error) {
	panic("unexpected ListByUser call")
}
func (r *signupInvitationRedeemRepoStub) ListByUserPaginated(context.Context, int64, pagination.PaginationParams, string) ([]RedeemCode, *pagination.PaginationResult, error) {
	panic("unexpected ListByUserPaginated call")
}
func (r *signupInvitationRedeemRepoStub) SumPositiveBalanceByUser(context.Context, int64) (float64, error) {
	panic("unexpected SumPositiveBalanceByUser call")
}
