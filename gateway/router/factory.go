package router

import (
	"fmt"
	"time"

	"github.com/prorochestvo/yarddog/domain"
	"github.com/prorochestvo/yarddog/services"
)

// New builds the router driver selected by kind (design §14: the reboot
// path is a device-adapter family, not a single client). Empty kind
// defaults to nokia, matching domain.ParseRouterKind's own default so a
// caller that bypasses config validation still gets the historical
// behavior.
func New(kind domain.RouterKind, cfg RouterConfig) (services.Rebooter, error) {
	switch kind {
	case domain.RouterKindNokia, "":
		return NewNokiaDriver(cfg.Addr, cfg.User, cfg.Pass, cfg.Timeout)
	default:
		return nil, fmt.Errorf("router: unsupported ROUTER_KIND %q", kind)
	}
}

// NewCredentialer builds a services.Credentialer for the device driver selected
// by kind (the credential-probe sibling of New). Kept as a separate factory so
// the daemon's composition root never holds a services.Rebooter — it receives
// only the narrower Credentialer interface, making it structurally incapable of
// rebooting. The dispatch mirrors New exactly; adding a new device kind adds a
// case in both factories.
func NewCredentialer(kind domain.RouterKind, cfg RouterConfig) (services.Credentialer, error) {
	switch kind {
	case domain.RouterKindNokia, "":
		return NewNokiaDriver(cfg.Addr, cfg.User, cfg.Pass, cfg.Timeout)
	default:
		return nil, fmt.Errorf("router: unsupported ROUTER_KIND %q", kind)
	}
}

// RouterConfig is the narrow set of connection parameters every driver in
// this family needs; New destructures it into whichever concrete
// constructor kind selects.
type RouterConfig struct {
	Addr    string
	User    string
	Pass    string
	Timeout time.Duration
}
