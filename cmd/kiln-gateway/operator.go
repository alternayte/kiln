package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/alternayte/auth-all/apierr"
	"github.com/alternayte/auth-all/plugins/admin"
	authstore "github.com/alternayte/auth-all/store"
)

// ensureOperator makes the first operator of a deployment, or repairs one.
//
// A fresh gateway holds no operator, and the operator routes need one. The
// two variables are read at every start: the operator is created when it is
// missing, its role is raised to operator, and the password is set when
// KILN_OPERATOR_PASSWORD holds one. Both variables can then be removed.
func ensureOperator(ctx context.Context, adm *admin.Plugin, store authstore.Store, role string) error {
	email := strings.TrimSpace(os.Getenv("KILN_OPERATOR_EMAIL"))
	if email == "" {
		return nil
	}
	password := os.Getenv("KILN_OPERATOR_PASSWORD")

	user, err := store.Users().GetByNormalizedEmail(ctx, strings.ToLower(email))
	switch {
	case err == nil:
	case errors.Is(err, authstore.ErrNotFound), errors.Is(err, apierr.ErrNotFound):
		if password == "" {
			return fmt.Errorf("operator: %s does not exist, so KILN_OPERATOR_PASSWORD is required", email)
		}
		created, _, err := adm.CreateUser(ctx, admin.CreateUserInput{
			Email: email, Password: password, Role: role,
		})
		if err != nil {
			return fmt.Errorf("operator: create %s: %w", email, err)
		}
		fmt.Printf("kiln-gateway: operator %s created\n", email)
		user = created
		password = ""
	default:
		return fmt.Errorf("operator: read %s: %w", email, err)
	}

	if user.Role != role {
		if _, err := adm.SetRole(ctx, user.ID, role); err != nil {
			return fmt.Errorf("operator: set the role of %s: %w", email, err)
		}
		fmt.Printf("kiln-gateway: operator %s now holds the role %s\n", email, role)
	}
	if password != "" {
		if _, err := adm.ResetPassword(ctx, user.ID, admin.ResetOptions{Password: password}); err != nil {
			return fmt.Errorf("operator: set the password of %s: %w", email, err)
		}
		fmt.Printf("kiln-gateway: operator %s has a new password\n", email)
	}
	return nil
}
