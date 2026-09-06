package appliance

import (
	"context"
	"fmt"
	"log"

	"github.com/tiny125/vm-replication/internal/linode"
)

// ---- F-30: name the Linode account service-limit arithmetic --------------
//
// A cutover (or a migration create) provisions Linode instances and Block
// Storage volumes, both of which count against the account-wide "active
// services" cap. Hitting that cap used to surface as a raw, late error:
//
//	cutover failed: clone data disk 1 into f14-twodisk-cutover-1:
//	linode POST /volumes/17756356/clone: 400 Bad Request:
//	{"errors": [{"reason": "You've reached a limit for the number of active
//	 services on your account. Please contact Support to request an increase
//	 and provide the total number of services you may need."}]}
//
// — arriving only after the full replication, boot conversion, and
// destination creation had already run, and never saying what vm-replication
// actually needed.
//
// Checked against internal/linode (no such field on any Client method) and
// the public Linode API v4 reference: there is no endpoint or /account field
// that exposes the account's active-services CAP as a number — the only place
// it appears at all is this error's free-text reason. That rules out a real
// pre-flight validation (there is nothing to validate against): the two
// things below only ever state the REQUIREMENT and the CURRENT count, never a
// pass/fail verdict against a limit vm-replication cannot actually read. A
// gate that guessed the cap would be worse than the bug it replaces — it
// would eventually block a legitimate cutover on a wrong guess.

// serviceUsage is a snapshot of how many Linode instances and Block Storage
// volumes the account currently holds.
type serviceUsage struct {
	Instances int
	Volumes   int
}

func (u serviceUsage) total() int { return u.Instances + u.Volumes }

// serviceNeed is how many NEW instances/volumes one cutover (or one
// migration-create call) will provision — the cutover's total requirement,
// not a remaining-from-here count, so the up-front announcement and a later
// failure both quote the same, stable numbers for the same run.
type serviceNeed struct {
	Instances int
	Volumes   int
}

func (n serviceNeed) total() int { return n.Instances + n.Volumes }

// linodeServiceUsage counts the account's current instances and volumes with
// two cheap list calls. page_size=500 catches any realistic account in one
// page, matching the same convention already used by ListTypes/ListBuckets/
// ListDisks elsewhere in this codebase.
func linodeServiceUsage(ctx context.Context, cl *linode.Client) (serviceUsage, error) {
	insts, err := cl.ListInstances(ctx)
	if err != nil {
		return serviceUsage{}, fmt.Errorf("list instances: %w", err)
	}
	vols, err := cl.ListVolumes(ctx)
	if err != nil {
		return serviceUsage{}, fmt.Errorf("list volumes: %w", err)
	}
	return serviceUsage{Instances: len(insts), Volumes: len(vols)}, nil
}

// serviceLimitMessage explains a service-limit hit in terms an operator can
// act on: how many services this step needed, and how many the account
// already has. It deliberately does not — and cannot — state headroom against
// a cap, because Linode's API never exposes that number (see the package
// comment above).
func serviceLimitMessage(need serviceNeed, cur serviceUsage, cause error) string {
	return fmt.Sprintf(
		"the Linode account has reached its limit on active services. This step needs %d more (%d instance(s), %d volume(s)); the account currently has %d active (%d instance(s), %d volume(s)). Free capacity — delete an unused instance or volume — or ask Linode Support to raise the limit, then retry. Linode said: %v",
		need.total(), need.Instances, need.Volumes, cur.total(), cur.Instances, cur.Volumes, cause)
}

// wrapServiceLimit rewrites err into an actionable message when it is a
// service-limit hit (linode.IsServiceLimit); any other error — including a
// failure to look up current usage — is returned unchanged, or (for a usage
// lookup failure) augmented with only the part that's known for certain: this
// is a diagnostic enhancement, and it must never hide, replace, or guess at
// the operator's real error.
func wrapServiceLimit(ctx context.Context, cl *linode.Client, err error, need serviceNeed) error {
	if !linode.IsServiceLimit(err) {
		return err
	}
	cur, uerr := linodeServiceUsage(ctx, cl)
	if uerr != nil {
		return fmt.Errorf(
			"the Linode account has reached its limit on active services. This step needs %d more (%d instance(s), %d volume(s)); could not read the account's current usage to report it (%v). Free capacity or ask Linode Support to raise the limit, then retry. Linode said: %v",
			need.total(), need.Instances, need.Volumes, uerr, err)
	}
	return fmt.Errorf("%s", serviceLimitMessage(need, cur, err))
}

// announceCutoverServiceNeed emits an up-front, informational activity event
// at the start of a cutover (or before provisioning a migration's replication
// volume) stating how many additional Linode services this run will
// provision and how many the account currently has — so an operator close to
// the account-wide cap can act BEFORE the expensive work (replication, boot
// conversion, destination creation) instead of discovering it only after
// landing in `failed` at the last clone (F-30). Best-effort and read-only: if
// the usage lookup fails, this logs and skips rather than blocking the
// cutover or posting a misleading (zero-filled) count.
func (s *Server) announceCutoverServiceNeed(ctx context.Context, migID int64, cl *linode.Client, need serviceNeed) {
	cur, err := linodeServiceUsage(ctx, cl)
	if err != nil {
		log.Printf("appliance: migration %d: could not read current Linode service usage: %v", migID, err)
		return
	}
	_ = s.st.AddEvent(s.ctx, migID, "info", fmt.Sprintf(
		"cutover: this run will provision %d additional Linode service(s) — %d instance(s) and %d volume(s). The account currently has %d active (%d instance(s), %d volume(s)); if Linode's account-wide service limit is close, free up capacity before this reaches the final clone.",
		need.total(), need.Instances, need.Volumes, cur.total(), cur.Instances, cur.Volumes))
}
