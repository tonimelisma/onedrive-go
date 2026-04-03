package cli

import (
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

type syncControlService struct {
	cc  *CLIContext
	now func() time.Time
}

func newSyncControlService(cc *CLIContext) *syncControlService {
	return &syncControlService{
		cc:  cc,
		now: time.Now,
	}
}

func (s *syncControlService) runPause(args []string) error {
	driveSelector, driveErr := s.cc.Flags.SingleDrive()
	if driveErr != nil {
		return driveErr
	}

	if driveSelector == "" {
		return fmt.Errorf("--drive is required (specify which drive to pause)")
	}

	cfg, err := config.LoadOrDefault(s.cc.CfgPath, s.cc.Logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	cid, err := driveid.NewCanonicalID(driveSelector)
	if err != nil {
		return fmt.Errorf("invalid drive ID %q: %w", driveSelector, err)
	}

	if _, exists := cfg.Drives[cid]; !exists {
		return fmt.Errorf("drive %q not found in config", driveSelector)
	}

	if err := config.SetDriveKey(s.cc.CfgPath, cid, "paused", "true"); err != nil {
		return fmt.Errorf("setting paused flag: %w", err)
	}

	if len(args) > 0 {
		duration, err := parseDuration(args[0])
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", args[0], err)
		}

		until := s.now().Add(duration).Format(time.RFC3339)
		if err := config.SetDriveKey(s.cc.CfgPath, cid, "paused_until", until); err != nil {
			return fmt.Errorf("setting paused_until: %w", err)
		}

		s.cc.Statusf("Drive %s paused until %s\n", cid.String(), until)
	} else {
		s.cc.Statusf("Drive %s paused\n", cid.String())
	}

	notifyDaemon(s.cc)

	return nil
}

func (s *syncControlService) runResume() error {
	cfg, err := config.LoadOrDefault(s.cc.CfgPath, s.cc.Logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	driveSelector, driveErr := s.cc.Flags.SingleDrive()
	if driveErr != nil {
		return driveErr
	}

	if driveSelector != "" {
		return s.resumeSingleDrive(cfg, driveSelector)
	}

	return s.resumeAllDrives(cfg)
}

func (s *syncControlService) resumeSingleDrive(cfg *config.Config, selector string) error {
	cid, err := driveid.NewCanonicalID(selector)
	if err != nil {
		return fmt.Errorf("invalid drive ID %q: %w", selector, err)
	}

	d, exists := cfg.Drives[cid]
	if !exists {
		return fmt.Errorf("drive %q not found in config", selector)
	}

	if !d.IsPaused(s.now()) {
		if d.Paused != nil && *d.Paused {
			if err := clearPausedKeys(s.cc.CfgPath, cid); err != nil {
				return err
			}

			s.cc.Statusf("Drive %s: expired timed pause cleared\n", cid.String())
			return nil
		}

		s.cc.Statusf("Drive %s is not paused\n", cid.String())
		return nil
	}

	if err := clearPausedKeys(s.cc.CfgPath, cid); err != nil {
		return err
	}

	s.cc.Statusf("Drive %s resumed\n", cid.String())
	notifyDaemon(s.cc)

	return nil
}

func (s *syncControlService) resumeAllDrives(cfg *config.Config) error {
	if len(cfg.Drives) == 0 {
		return fmt.Errorf("no drives configured")
	}

	resumed := 0
	for cid := range cfg.Drives {
		d := cfg.Drives[cid]
		if !d.IsPaused(s.now()) {
			if d.Paused != nil && *d.Paused {
				if err := clearPausedKeys(s.cc.CfgPath, cid); err != nil {
					return fmt.Errorf("clearing expired pause for %s: %w", cid.String(), err)
				}

				s.cc.Statusf("Drive %s: expired timed pause cleared\n", cid.String())
				resumed++
			}

			continue
		}

		if err := clearPausedKeys(s.cc.CfgPath, cid); err != nil {
			return fmt.Errorf("resuming %s: %w", cid.String(), err)
		}

		s.cc.Statusf("Drive %s resumed\n", cid.String())
		resumed++
	}

	if resumed == 0 {
		s.cc.Statusf("No paused drives found\n")
		return nil
	}

	notifyDaemon(s.cc)

	return nil
}
