package v1alpha1

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/cockroachdb/errors"
)

const (
	// TenantSystemID is the ID of the internal, privileged system tenant.
	TenantSystemID TenantID = 1

	// TenantMinID is the minimum ID of a (non-system) tenant in a multi-tenant
	// CockroachDB cluster. The TenantID of 1 corresponds to the ID for the
	// system tenant.
	TenantMinID = TenantID(2)

	// TenantMaxID is the maximum ID of a (non-system) tenant in a multi-tenant
	// CockroachDB cluster.
	TenantMaxID = TenantID(math.MaxUint64)

	// TenantNilID corresponds to an invalid tenant ID.
	TenantNilID = TenantID(0)

	tenantTagPrefix = "tenant-"
)

// TenantID is a unique ID associated with each tenant in a multi-tenant
// CockroachDB cluster.
//
// For more information, see:
//
//	https://github.com/cockroachdb/cockroach/blob/release-20.2/pkg/roachpb/tenant.go#L19-L24
type TenantID uint64

// IsSystemID returns true if this is the internal, privileged system tenant.
// The system tenant has administrative access and can create and destroy other
// tenants.
func (t TenantID) IsSystemID() bool {
	return t == TenantSystemID
}

// Tag returns the tenant's "tag", which looks like this:
//
//	tenant-50
//
// It is commonly used as the name of a CrdbTenant object.
func (t TenantID) Tag() string {
	return fmt.Sprintf("%s%d", tenantTagPrefix, t)
}

// ToUint64 returns the TenantID as a uint64.
func (t TenantID) ToUint64() uint64 {
	return uint64(t)
}

func (t TenantID) String() string {
	return strconv.FormatUint(uint64(t), 10)
}

// IsValid returns whether the TenantID is valid.
func (t TenantID) IsValid() bool {
	return t != TenantNilID
}

// ParseTenantID constructs a new TenantID from the provided string.
func ParseTenantID(str string) (TenantID, error) {
	id, err := strconv.ParseUint(str, 10 /* base */, 64 /* bitSize */)
	if err != nil {
		return TenantNilID, errors.Wrap(err, "parsing TenantID")
	}
	t := TenantID(id)
	if !t.IsValid() {
		return TenantNilID, errors.New("TenantID is invalid")
	}
	return t, nil
}

// ParseTenantTag constructs a new TenantID from the .Tag form (tenant-%d) of
// a TenantID.
func ParseTenantTag(str string) (TenantID, error) {
	if !strings.HasPrefix(str, tenantTagPrefix) {
		return TenantNilID, errors.Newf("expected a string starting with %s. Got '%s'", tenantTagPrefix, str)
	}
	return ParseTenantID(str[len(tenantTagPrefix):])
}
