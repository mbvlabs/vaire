package doctor

// ActivationChecks are the durable activation state checks.
func ActivationChecks() []Check {
	return []Check{
		{ID: "activation.pending", Category: CatPostgres, Run: activationPending},
	}
}

// activationPending reports a database whose activation fence or unfollow has
// not finished. A running PostgreSQL says nothing about client readiness: a
// routing fence still has to verify both poolers' routes and release their
// pause, and an unsettled unfollow still withholds the pooler alias.
func activationPending(f Facts) []Result {
	out := make([]Result, 0, len(f.Clusters))
	for _, c := range f.Clusters {
		r := Result{DB: c.Name}
		fence, fenced := f.Records.ActivationFenceFor(c.Name)
		unfollow, unfollowing := f.Records.Unfollowing[c.Name]
		switch {
		case fenced && fence.Phase == "routing":
			r.Status = Fail
			r.Detail = "the " + fence.Kind + " activation " + fence.OperationID +
				" is still in routing: its pooler routes and pause release are not verified, so a running cluster is not verified as ready for clients"
			r.Fix = "vaire apply --db " + c.Name
		case fenced:
			r.Status = Fail
			r.Detail = "the " + fence.Kind + " activation " + fence.OperationID +
				" is unfinished in phase " + fence.Phase + "; client routing stays withheld"
			r.Fix = "finish or resume the " + fence.Kind + " operation " + fence.OperationID
		case unfollowing && !unfollow.Settled:
			r.Status = Fail
			r.Detail = "the unfollow from " + unfollow.Source + " has not settled, so the poolers keep " + c.Name + "'s aliases withheld"
			r.Fix = "vaire db unfollow " + c.Name
		case c.AwaitingRecovery:
			r.Status, r.Detail = Skip, "awaiting recovery (vaire db recover "+c.Name+")"
		default:
			r.Status, r.Detail = Pass, "no unfinished activation or routing"
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return one(Skip, "no databases", "")
	}
	return out
}
