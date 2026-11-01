package sync

import (
	"fmt"
	"testing"
)

// BenchmarkBuildDependencies times the dependency build over the action
// lists a plan of a large drive produces, at 10k and 100k actions:
//
//   - upload: a first sync of a local tree into an empty OneDrive, 10 files
//     per folder in 100 folders per top folder (a remote folder create per
//     folder, an upload per file: the shape of the live 143k-action plan);
//   - mixed: the same tree where every tenth folder is renamed on one side
//     (a folder move with downloads and remote deletes under its source and
//     destination), every tenth is deleted on one side with its children,
//     and every tenth file is a case-only rename OneDrive cannot pair (a
//     remote delete plus an upload of the case variant).
//
// BenchmarkBuildDependenciesScanningOracle is the implementation this one
// replaced (oracleBuildDependencies in planner_dependencies_test.go), at 10k
// only: it is quadratic, 100k takes a quarter of an hour.
//
// Run with: go test ./internal/sync -run '^$' -bench 'BenchmarkBuildDependencies/actions=10000/' -benchtime 1x

type dependencyBenchShape string

const (
	benchDependencyShapeUpload dependencyBenchShape = "upload"
	benchDependencyShapeMixed  dependencyBenchShape = "mixed"
)

const (
	benchDependencyFilesPerFolder = 10
	benchDependencyFoldersPerTop  = 100
)

// Validates: R-6.1.3
func BenchmarkBuildDependencies(b *testing.B) {
	for _, actionCount := range []int{10_000, 100_000} {
		for _, shape := range []dependencyBenchShape{benchDependencyShapeUpload, benchDependencyShapeMixed} {
			b.Run(fmt.Sprintf("actions=%d/%s", actionCount, shape), func(b *testing.B) {
				actions := benchDependencyActions(actionCount, shape)

				b.ResetTimer()
				for b.Loop() {
					deps := buildDependencies(actions)
					edges := 0
					for i := range deps {
						edges += len(deps[i])
					}
					b.ReportMetric(float64(len(actions)), "actions")
					b.ReportMetric(float64(edges), "edges")
				}
			})
		}
	}
}

func BenchmarkBuildDependenciesScanningOracle(b *testing.B) {
	for _, shape := range []dependencyBenchShape{benchDependencyShapeUpload, benchDependencyShapeMixed} {
		b.Run(fmt.Sprintf("actions=%d/%s", 10_000, shape), func(b *testing.B) {
			actions := benchDependencyActions(10_000, shape)

			b.ResetTimer()
			for b.Loop() {
				oracleBuildDependencies(actions)
				b.ReportMetric(float64(len(actions)), "actions")
			}
		})
	}
}

// benchDependencyActions builds about actionCount actions of the given shape.
func benchDependencyActions(actionCount int, shape dependencyBenchShape) []Action {
	views := newBenchDependencyViews()
	actions := make([]Action, 0, actionCount+benchDependencyFoldersPerTop)
	folders := actionCount / (benchDependencyFilesPerFolder + 1)
	for f := range folders {
		top := f / benchDependencyFoldersPerTop
		if f%benchDependencyFoldersPerTop == 0 {
			actions = append(actions, Action{Type: ActionFolderCreate, Path: fmt.Sprintf("top-%03d", top), CreateSide: CreateRemote})
		}
		folderPath := fmt.Sprintf("top-%03d/folder-%06d", top, f)
		if shape == benchDependencyShapeMixed {
			switch f % 10 {
			case 1:
				actions = views.appendRemotelyRenamedFolder(actions, folderPath)
				continue
			case 2:
				actions = views.appendLocallyDeletedFolder(actions, folderPath)
				continue
			case 3:
				actions = views.appendLocallyRenamedFolder(actions, folderPath, top, f)
				continue
			}
		}
		actions = views.appendUploadedFolder(actions, folderPath, shape)
	}

	return actions
}

// benchDependencyViews are the path views the shapes attach: moves carry
// the moved item's baseline type, deletes the deleted item's type.
type benchDependencyViews struct {
	folder, file, deletedFolder, deletedFile *pathView
}

func newBenchDependencyViews() benchDependencyViews {
	return benchDependencyViews{
		folder: &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFolder}},
		file:   &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFile}},
		deletedFolder: &pathView{
			Local:    &localState{ItemType: ItemTypeFolder},
			Remote:   &remoteState{ItemType: ItemTypeFolder},
			Baseline: &BaselineEntry{ItemType: ItemTypeFolder},
		},
		deletedFile: &pathView{
			Local:    &localState{ItemType: ItemTypeFile},
			Remote:   &remoteState{ItemType: ItemTypeFile},
			Baseline: &BaselineEntry{ItemType: ItemTypeFile},
		},
	}
}

// appendUploadedFolder is a new folder: a remote folder create and an
// upload per file; in the mixed shape its first file is a case-only rename
// OneDrive could not pair as a move (a remote delete plus an upload).
func (views benchDependencyViews) appendUploadedFolder(actions []Action, folderPath string, shape dependencyBenchShape) []Action {
	actions = append(actions, Action{Type: ActionFolderCreate, Path: folderPath, CreateSide: CreateRemote})
	for i := range benchDependencyFilesPerFolder {
		path := fmt.Sprintf("%s/file-%02d.txt", folderPath, i)
		if shape == benchDependencyShapeMixed && i == 0 {
			actions = append(actions,
				Action{Type: ActionRemoteDelete, Path: path, View: views.deletedFile},
				Action{Type: ActionUpload, Path: fmt.Sprintf("%s/FILE-%02d.txt", folderPath, i)})
			continue
		}
		actions = append(actions, Action{Type: ActionUpload, Path: path})
	}

	return actions
}

// appendRemotelyRenamedFolder: OneDrive renamed the folder, so a local
// move with downloads under the new name, while the local side had deleted
// two children, addressed at the old path.
func (views benchDependencyViews) appendRemotelyRenamedFolder(actions []Action, oldPath string) []Action {
	folderPath := oldPath + "-renamed"
	actions = append(actions, Action{Type: ActionLocalMove, OldPath: oldPath, Path: folderPath, View: views.folder})
	for i := range benchDependencyFilesPerFolder {
		if i < 2 {
			actions = append(actions, Action{Type: ActionRemoteDelete, Path: fmt.Sprintf("%s/file-%02d.txt", oldPath, i), View: views.deletedFile})
			continue
		}
		actions = append(actions, Action{Type: ActionDownload, Path: fmt.Sprintf("%s/file-%02d.txt", folderPath, i), View: views.file})
	}

	return actions
}

// appendLocallyDeletedFolder: the folder was deleted locally, so a
// remote delete of it and of each child.
func (views benchDependencyViews) appendLocallyDeletedFolder(actions []Action, folderPath string) []Action {
	actions = append(actions, Action{Type: ActionRemoteDelete, Path: folderPath, View: views.deletedFolder})
	for i := range benchDependencyFilesPerFolder {
		actions = append(actions, Action{Type: ActionRemoteDelete, Path: fmt.Sprintf("%s/file-%02d.txt", folderPath, i), View: views.deletedFile})
	}

	return actions
}

// appendLocallyRenamedFolder: the folder was renamed locally, so a
// remote move with uploads under the new name, one child moved out first.
func (views benchDependencyViews) appendLocallyRenamedFolder(actions []Action, oldPath string, top, f int) []Action {
	folderPath := oldPath + "-Renamed"
	actions = append(actions,
		Action{Type: ActionRemoteMove, OldPath: oldPath, Path: folderPath, View: views.folder},
		Action{Type: ActionRemoteMove, OldPath: oldPath + "/file-00.txt", Path: fmt.Sprintf("top-%03d/moved-out-%06d.txt", top, f), View: views.file})
	for i := 1; i < benchDependencyFilesPerFolder; i++ {
		actions = append(actions, Action{Type: ActionUpload, Path: fmt.Sprintf("%s/file-%02d.txt", folderPath, i)})
	}

	return actions
}
