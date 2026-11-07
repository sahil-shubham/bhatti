package server

import "context"

// detached keeps ctx's values (the request ID, the user) but not its
// cancellation. A store write that records or undoes something already done to
// a VM must land even if the client that asked has gone away: abandoning it
// would leave the record and the VM disagreeing about what exists.
func detached(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }
