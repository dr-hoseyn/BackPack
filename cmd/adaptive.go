package cmd

import (
	"errors"
	"runtime"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tunnel/adaptive"
)

func checkManagedPaths(c *config.Config) error {
	if (len(c.Server.Paths) != 0 || len(c.Client.Paths) != 0) && runtime.GOOS != "linux" {
		return errors.New("managed paths require Linux")
	}
	return adaptive.Validate(c)
}
