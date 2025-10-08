/*
Copyright 2018 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	kConfig "github.com/osscontainertools/kaniko/pkg/config"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/osscontainertools/kaniko/pkg/dockerfile"
	"github.com/osscontainertools/kaniko/pkg/util"
)

// for testing
var (
	getUserGroup       = util.GetUserGroup
	getActiveUserGroup = util.GetActiveUserGroup
)

type CopyCommand struct {
	BaseCommand
	cmd           *instructions.CopyCommand
	fileContext   util.FileContext
	snapshotFiles []string
	shdCache      bool
}

func (c *CopyCommand) ExecuteCommand(config *v1.Config, buildArgs *dockerfile.BuildArgs) error {
	// Resolve from
	uid, gid := int64(-1), int64(-1)
	var err error
	replacementEnvs := buildArgs.ReplacementEnvs(config.Env)
	if c.cmd.From != "" {
		c.fileContext = util.FileContext{Root: filepath.Join(kConfig.KanikoInterStageDepsDir, c.cmd.From)}
		uid, gid, err = getUserGroup(c.cmd.Chown, replacementEnvs)
		if err != nil {
			return errors.Wrap(err, "getting user group from chown")
		}
	} else {
		user := config.User
		if kConfig.EnvBool("FF_KANIKO_COPY_AS_ROOT") {
			// According to spec: https://docs.docker.com/reference/dockerfile/#copy---chown---chmod
			//   All files and directories copied from the build context
			//   are created with a default UID and GID of 0.
			// But this is a breaking change so we keep it optional for now
			user = "0:0"
		}
		uid, gid, err = getActiveUserGroup(user, c.cmd.Chown, replacementEnvs)
		if err != nil {
			return errors.Wrap(err, "getting user group from chown")
		}
	}

	// sources from the Copy command are resolved with wildcards {*?[}
	srcs, dest, err := util.ResolveEnvAndWildcards(c.cmd.SourcesAndDest, c.fileContext, replacementEnvs)
	if err != nil {
		return errors.Wrap(err, "resolving src")
	}

	chmod, useDefaultChmod, err := util.GetChmod(c.cmd.Chmod, replacementEnvs)
	if err != nil {
		return errors.Wrap(err, "getting permissions from chmod")
	}

	// For each source, iterate through and copy it over
	for _, src := range srcs {
		fullPath := filepath.Join(c.fileContext.Root, src)

		fi, err := os.Lstat(fullPath)
		if err != nil {
			return errors.Wrap(err, "could not copy source")
		}
		if fi.IsDir() && !strings.HasSuffix(fullPath, string(os.PathSeparator)) {
			fullPath += "/"
		}
		cwd := config.WorkingDir
		if cwd == "" {
			cwd = kConfig.RootDir
		}

		destPath, err := util.DestinationFilepath(fullPath, dest, cwd)
		if err != nil {
			return errors.Wrap(err, "find destination path")
		}

		// If the destination dir is a symlink we need to resolve the path and use
		// that instead of the symlink path
		destPath, err = resolveIfSymlink(destPath)
		if err != nil {
			return errors.Wrap(err, "resolving dest symlink")
		}

		if fi.IsDir() {
			copiedFiles, err := util.CopyDir(fullPath, destPath, c.fileContext, uid, gid, chmod, useDefaultChmod)
			if err != nil {
				return errors.Wrap(err, "copying dir")
			}
			c.snapshotFiles = append(c.snapshotFiles, copiedFiles...)
		} else if util.IsSymlink(fi) {
			// If file is a symlink, we want to copy the target file to destPath
			exclude, err := util.CopySymlink(fullPath, destPath, c.fileContext)
			if err != nil {
				return errors.Wrap(err, "copying symlink")
			}
			if exclude {
				continue
			}
			c.snapshotFiles = append(c.snapshotFiles, destPath)
		} else {
			// ... Else, we want to copy over a file
			exclude, err := util.CopyFile(fullPath, destPath, c.fileContext, uid, gid, chmod, useDefaultChmod)
			if err != nil {
				return errors.Wrap(err, "copying file")
			}
			if exclude {
				continue
			}
			c.snapshotFiles = append(c.snapshotFiles, destPath)
		}
	}

	// Heredocs
	for _, src := range c.cmd.SourcesAndDest.SourceContents {
		fullPath := filepath.Join(c.fileContext.Root, src.Path)
		cwd := config.WorkingDir
		if cwd == "" {
			cwd = kConfig.RootDir
		}
		destPath, err := util.DestinationFilepath(fullPath, dest, cwd)
		if err != nil {
			return errors.Wrap(err, "find destination path")
		}

		srcFile := strings.NewReader(src.Data)
		err = util.CreateFile(destPath, srcFile, chmod, uint32(uid), uint32(gid))
		if err != nil {
			return errors.Wrap(err, "creating file")
		}
		c.snapshotFiles = append(c.snapshotFiles, destPath)
	}

	return nil
}

// FilesToSnapshot should return an empty array if still nil; no files were changed
func (c *CopyCommand) FilesToSnapshot() []string {
	return c.snapshotFiles
}

// String returns some information about the command for the image config
func (c *CopyCommand) String() string {
	return c.cmd.String()
}

func (c *CopyCommand) FilesUsedFromContext(config *v1.Config, buildArgs *dockerfile.BuildArgs) ([]string, error) {
	return copyCmdFilesUsedFromContext(config, buildArgs, c.cmd, c.fileContext)
}

func (c *CopyCommand) MetadataOnly() bool {
	return false
}

func (c *CopyCommand) RequiresUnpackedFS() bool {
	return true
}

func (c *CopyCommand) From() string {
	return c.cmd.From
}

func (c *CopyCommand) ShouldCacheOutput() bool {
	return c.shdCache
}

// CacheCommand returns true since this command should be cached
func (c *CopyCommand) CacheCommand(img v1.Image) DockerCommand {
	return &CachingCopyCommand{
		img:         img,
		cmd:         c.cmd,
		fileContext: c.fileContext,
		extractFn:   util.ExtractFile,
	}
}

type CachingCopyCommand struct {
	BaseCommand
	caching
	img            v1.Image
	extractedFiles []string
	cmd            *instructions.CopyCommand
	fileContext    util.FileContext
	extractFn      util.ExtractFunction
}

func (cr *CachingCopyCommand) ExecuteCommand(config *v1.Config, buildArgs *dockerfile.BuildArgs) error {
	logrus.Infof("Found cached layer, extracting to filesystem")
	var err error

	if cr.img == nil {
		return errors.New(fmt.Sprintf("cached command image is nil %v", cr.String()))
	}

	layers, err := cr.img.Layers()
	if err != nil {
		return errors.Wrapf(err, "retrieve image layers")
	}

	if len(layers) != 1 {
		return errors.New(fmt.Sprintf("expected %d layers but got %d", 1, len(layers)))
	}

	cr.layer = layers[0]
	cr.extractedFiles, err = util.GetFSFromLayers(kConfig.RootDir, layers, util.ExtractFunc(cr.extractFn), util.IncludeWhiteout())

	logrus.Debugf("ExtractedFiles: %s", cr.extractedFiles)
	if err != nil {
		return errors.Wrap(err, "extracting fs from image")
	}

	return nil
}

func (cr *CachingCopyCommand) FilesUsedFromContext(config *v1.Config, buildArgs *dockerfile.BuildArgs) ([]string, error) {
	return copyCmdFilesUsedFromContext(config, buildArgs, cr.cmd, cr.fileContext)
}

func (cr *CachingCopyCommand) FilesToSnapshot() []string {
	f := cr.extractedFiles
	logrus.Debugf("%d files extracted by caching copy command", len(f))
	logrus.Tracef("Extracted files: %s", f)

	return f
}

func (cr *CachingCopyCommand) MetadataOnly() bool {
	return false
}

func (cr *CachingCopyCommand) String() string {
	if cr.cmd == nil {
		return "nil command"
	}
	return cr.cmd.String()
}

func (cr *CachingCopyCommand) From() string {
	return cr.cmd.From
}

func resolveIfSymlink(destPath string) (string, error) {
	if !filepath.IsAbs(destPath) {
		return "", errors.New("dest path must be abs")
	}

	var nonexistentPaths []string

	newPath := destPath
	for newPath != "/" {
		_, err := os.Lstat(newPath)
		if err != nil {
			if os.IsNotExist(err) {
				dir, file := filepath.Split(newPath)
				newPath = filepath.Clean(dir)
				nonexistentPaths = append(nonexistentPaths, file)
				continue
			} else {
				return "", errors.Wrap(err, "failed to lstat")
			}
		}

		newPath, err = filepath.EvalSymlinks(newPath)
		if err != nil {
			return "", errors.Wrap(err, "failed to eval symlinks")
		}
		break
	}

	for i := len(nonexistentPaths) - 1; i >= 0; i-- {
		newPath = filepath.Join(newPath, nonexistentPaths[i])
	}

	if destPath != newPath {
		logrus.Tracef("Updating destination path from %v to %v due to symlink", destPath, newPath)
	}

	return filepath.Clean(newPath), nil
}

func copyCmdFilesUsedFromContext(
	config *v1.Config, buildArgs *dockerfile.BuildArgs, cmd *instructions.CopyCommand,
	fileContext util.FileContext,
) ([]string, error) {
	if cmd.From != "" {
		fileContext = util.FileContext{Root: filepath.Join(kConfig.KanikoInterStageDepsDir, cmd.From)}
	}

	replacementEnvs := buildArgs.ReplacementEnvs(config.Env)

	srcs, _, err := util.ResolveEnvAndWildcards(
		cmd.SourcesAndDest, fileContext, replacementEnvs,
	)
	if err != nil {
		return nil, err
	}

	files := []string{}
	for _, src := range srcs {
		fullPath := filepath.Join(fileContext.Root, src)
		files = append(files, fullPath)
	}

	logrus.Debugf("Using files from context: %v", files)

	return files, nil
}

// AbstractCopyCommand can either be a CopyCommand or a CachingCopyCommand.
type AbstractCopyCommand interface {
	From() string
}

// CastAbstractCopyCommand tries to convert a command to an AbstractCopyCommand.
func CastAbstractCopyCommand(cmd interface{}) (AbstractCopyCommand, bool) {
	switch v := cmd.(type) {
	case *CopyCommand:
		return v, true
	case *CachingCopyCommand:
		return v, true
	}

	return nil, false
}
