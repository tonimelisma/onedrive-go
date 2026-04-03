package cli

import (
	"context"
	"fmt"
)

type verifyService struct {
	cc *CLIContext
}

func newVerifyService(cc *CLIContext) *verifyService {
	return &verifyService{cc: cc}
}

func (s *verifyService) run(ctx context.Context) error {
	syncDir := s.cc.Cfg.SyncDir
	if syncDir == "" {
		return fmt.Errorf("sync_dir not configured — set it in the config file or add a drive with 'onedrive-go drive add'")
	}

	dbPath := s.cc.Cfg.StatePath()
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", s.cc.Cfg.CanonicalID)
	}

	report, err := loadAndVerify(ctx, dbPath, syncDir, s.cc.Logger)
	if err != nil {
		return err
	}

	if s.cc.Flags.JSON {
		if err := printVerifyJSON(s.cc.Output(), report); err != nil {
			return err
		}
	} else {
		if err := printVerifyTable(s.cc.Output(), report); err != nil {
			return err
		}
	}

	if len(report.Mismatches) > 0 {
		return errVerifyMismatch
	}

	return nil
}
