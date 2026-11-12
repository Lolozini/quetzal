package models

import "time"

// OrphanReport is what a controller found on one cluster the last time it
// looked for namespaces whose server is not in the database: the ones it will
// delete, volume included, after a grace period, or has refused to delete
// because there were too many at once. A restored database backup looks exactly
// like this, and the report is how an administrator hears of it before the
// volumes go.
type OrphanReport struct {
	ClusterID uint `json:"clusterId"`
	// Since is when this exact set was first reported.
	Since time.Time `json:"since"`
	// Names are the namespaces, sorted.
	Names []string `json:"names"`
	// Managed is how many namespaces of this instance the cluster holds, the
	// orphans included.
	Managed int `json:"managed"`
	// Refused is set when the controller will not delete them on its own: too
	// many of them at once, which is what an empty, wrong or restored database
	// looks like.
	Refused bool `json:"refused"`
	// GraceSeconds is how long a namespace stays unreferenced before it is
	// deleted; zero when it is deleted at once.
	GraceSeconds int64 `json:"graceSeconds"`
}
