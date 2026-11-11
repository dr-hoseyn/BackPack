// Package traffic retains the legacy sniffer API for existing configurations.
package traffic

import (
	"context"
	"github.com/sirupsen/logrus"
)

type Usage struct{}

func NewDataStore(_ context.Context, _ string, _ bool, _ *logrus.Logger) *Usage {
	return &Usage{}
}

// Run returns immediately. Per-port accounting has been removed.
func (*Usage) Run()                        {}
func (*Usage) AddOrUpdatePort(int, uint64) {}
