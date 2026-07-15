package service

import (
	"context"
	"errors"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

type signupInvitation struct {
	redeemCode    *RedeemCode
	affiliateCode string
}

func (i *signupInvitation) usesAffiliateCode() bool {
	return i != nil && i.affiliateCode != ""
}

// ValidateSignupInvitation accepts either a one-time invitation redeem code or
// an existing user's reusable affiliate code when invitation-only signup is on.
func (s *AuthService) ValidateSignupInvitation(ctx context.Context, invitationCode, affiliateCode string) error {
	_, err := s.resolveSignupInvitation(ctx, invitationCode, affiliateCode)
	return err
}

func (s *AuthService) resolveSignupInvitation(ctx context.Context, invitationCode, affiliateCode string) (*signupInvitation, error) {
	if s == nil || s.settingService == nil || !s.settingService.IsInvitationCodeEnabled(ctx) {
		return nil, nil
	}

	candidates := signupInvitationCandidates(invitationCode, affiliateCode)
	if len(candidates) == 0 {
		return nil, ErrInvitationCodeRequired
	}

	for _, code := range candidates {
		if s.redeemRepo != nil || s.oauthEmailFlowClient(ctx) != nil {
			redeemCode, err := s.loadOAuthRegistrationInvitation(ctx, code)
			switch {
			case err == nil && redeemCode.Type == RedeemTypeInvitation && redeemCode.CanUse():
				return &signupInvitation{redeemCode: redeemCode}, nil
			case err != nil && !errors.Is(err, ErrRedeemCodeNotFound):
				logger.LegacyPrintf("service.auth", "[Auth] Failed to validate signup invitation redeem code: %v", err)
				return nil, ErrServiceUnavailable
			}
		}

		if s.affiliateService == nil {
			continue
		}
		if _, err := s.affiliateService.lookupInviterByCode(ctx, code); err == nil {
			return &signupInvitation{affiliateCode: code}, nil
		} else if !errors.Is(err, ErrAffiliateCodeInvalid) && !errors.Is(err, ErrAffiliateProfileNotFound) {
			logger.LegacyPrintf("service.auth", "[Auth] Failed to validate signup affiliate code: %v", err)
			return nil, ErrServiceUnavailable
		}
	}

	return nil, ErrInvitationCodeInvalid
}

func signupInvitationCandidates(values ...string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		code := strings.TrimSpace(value)
		if code == "" {
			continue
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		result = append(result, code)
	}
	return result
}

func (s *AuthService) createUserWithSignupInvitation(ctx context.Context, user *User, invitation *signupInvitation) error {
	if s == nil || s.userRepo == nil {
		return ErrServiceUnavailable
	}
	if invitation == nil {
		return s.userRepo.Create(ctx, user)
	}

	create := func(opCtx context.Context) error {
		if err := s.userRepo.Create(opCtx, user); err != nil {
			return err
		}
		return s.applySignupInvitation(opCtx, user.ID, invitation)
	}

	if dbent.TxFromContext(ctx) != nil {
		resolved, err := s.resolveSignupInvitation(ctx, invitationCodeValue(invitation), "")
		if err != nil {
			return err
		}
		invitation = resolved
		return create(ctx)
	}
	if s.entClient == nil {
		if err := create(ctx); err != nil {
			if user != nil && user.ID > 0 {
				if rollbackErr := s.userRepo.Delete(ctx, user.ID); rollbackErr != nil {
					logger.LegacyPrintf("service.auth", "[Auth] Failed to rollback rejected signup user %d: %v", user.ID, rollbackErr)
				}
			}
			return err
		}
		return nil
	}

	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return ErrServiceUnavailable
	}
	defer func() { _ = tx.Rollback() }()
	txCtx := dbent.NewTxContext(ctx, tx)
	// Validate again inside the transaction so a concurrently consumed redeem
	// code cannot pass based on a stale pre-transaction lookup.
	resolved, err := s.resolveSignupInvitation(txCtx, invitationCodeValue(invitation), "")
	if err != nil {
		return err
	}
	invitation = resolved
	if err := create(txCtx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return ErrServiceUnavailable
	}
	return nil
}

func invitationCodeValue(invitation *signupInvitation) string {
	if invitation == nil {
		return ""
	}
	if invitation.redeemCode != nil {
		return invitation.redeemCode.Code
	}
	return invitation.affiliateCode
}

func (s *AuthService) applySignupInvitation(ctx context.Context, userID int64, invitation *signupInvitation) error {
	if invitation == nil {
		return nil
	}
	if invitation.redeemCode != nil {
		if err := s.useOAuthRegistrationInvitation(ctx, invitation.redeemCode.ID, userID); err != nil {
			if errors.Is(err, ErrRedeemCodeUsed) {
				return ErrInvitationCodeInvalid
			}
			return ErrServiceUnavailable
		}
		return nil
	}
	if invitation.affiliateCode == "" || s.affiliateService == nil {
		return ErrInvitationCodeInvalid
	}
	if err := s.affiliateService.bindSignupInviterByCode(ctx, userID, invitation.affiliateCode); err != nil {
		if errors.Is(err, ErrAffiliateCodeInvalid) || errors.Is(err, ErrAffiliateProfileNotFound) || errors.Is(err, ErrAffiliateAlreadyBound) {
			return ErrInvitationCodeInvalid
		}
		return ErrServiceUnavailable
	}
	return nil
}

func (s *AuthService) bindOptionalAffiliateAfterSignup(ctx context.Context, userID int64, affiliateCode string, invitation *signupInvitation) {
	if invitation.usesAffiliateCode() {
		return
	}
	s.bindOAuthAffiliate(ctx, userID, affiliateCode)
}
