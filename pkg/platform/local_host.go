package platform

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/fileutil"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

type LocalHost struct{}

func (LocalHost) EnsurePlatformAccounts(ctx context.Context, user string, group string, accessGroup string) (PlatformAccountIDs, error) {
	if runtime.GOOS != "linux" {
		return PlatformAccountIDs{}, fmt.Errorf("platform init is supported only on Linux hosts")
	}
	for _, command := range []string{"docker", "getent", "groupadd", "id", "systemctl", "useradd", "usermod"} {
		if _, err := exec.LookPath(command); err != nil {
			return PlatformAccountIDs{}, fmt.Errorf("platform init requires %s: %w", command, err)
		}
	}
	for _, value := range []string{user, group, accessGroup} {
		if !systemIdentifierPattern.MatchString(value) {
			return PlatformAccountIDs{}, fmt.Errorf("unsafe system account identifier %q", value)
		}
	}
	if user == "root" || group == "root" || accessGroup == "root" {
		return PlatformAccountIDs{}, fmt.Errorf("platform worker accounts must be dedicated and non-root")
	}
	for _, name := range []string{accessGroup, group} {
		if err := ensureSystemGroup(ctx, name); err != nil {
			return PlatformAccountIDs{}, err
		}
	}
	if err := validateUniqueNumericGroups(ctx, group, accessGroup); err != nil {
		return PlatformAccountIDs{}, err
	}
	if err := validateDedicatedWorkerGroup(ctx, group, user); err != nil {
		return PlatformAccountIDs{}, err
	}
	workerGroupGID, err := localGroupNumericID(ctx, group)
	if err != nil {
		return PlatformAccountIDs{}, err
	}
	accessGID, err := localGroupNumericID(ctx, accessGroup)
	if err != nil {
		return PlatformAccountIDs{}, err
	}
	if err := runLocalCommand(ctx, "id", "-u", user); err != nil {
		if err := runLocalCommand(ctx, "useradd", "--system", "--gid", group, "--groups", accessGroup, "--home-dir", DefaultStateDir, "--no-create-home", "--shell", "/usr/sbin/nologin", user); err != nil {
			return PlatformAccountIDs{}, fmt.Errorf("create platform worker user: %w", err)
		}
	} else {
		if err := validateExistingPlatformUser(ctx, user, group); err != nil {
			return PlatformAccountIDs{}, err
		}
		groupIDs, err := localNumericGroups(ctx, user)
		if err != nil {
			return PlatformAccountIDs{}, err
		}
		if err := validateWorkerNumericGroupsBeforeGrant(groupIDs, workerGroupGID, accessGID); err != nil {
			return PlatformAccountIDs{}, err
		}
		if err := runLocalCommand(ctx, "usermod", "-a", "-G", accessGroup, user); err != nil {
			return PlatformAccountIDs{}, fmt.Errorf("grant platform worker socket access: %w", err)
		}
	}
	if err := validateExistingPlatformUser(ctx, user, group); err != nil {
		return PlatformAccountIDs{}, err
	}
	if err := validateDedicatedWorkerGroup(ctx, group, user); err != nil {
		return PlatformAccountIDs{}, err
	}
	uid, err := localNumericID(ctx, "-u", user)
	if err != nil {
		return PlatformAccountIDs{}, err
	}
	gid, err := localNumericID(ctx, "-g", user)
	if err != nil {
		return PlatformAccountIDs{}, err
	}
	groupIDs, err := localNumericGroups(ctx, user)
	if err != nil {
		return PlatformAccountIDs{}, err
	}
	if err := validateWorkerNumericGroups(groupIDs, gid, accessGID); err != nil {
		return PlatformAccountIDs{}, err
	}
	return PlatformAccountIDs{WorkerUID: uid, WorkerGID: gid, SocketGroupGID: accessGID}, nil
}

func (LocalHost) ResolveDockerDataRoot(ctx context.Context, requested string) (string, error) {
	output, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.DockerRootDir}}").Output()
	if err != nil {
		return "", fmt.Errorf("inspect Docker data root: %w", err)
	}
	return validateDockerDataRoot(strings.TrimSpace(string(output)), requested)
}

func validateDockerDataRoot(reported string, requested string) (string, error) {
	actual := filepath.Clean(reported)
	if !filepath.IsAbs(actual) || strings.ContainsAny(actual, "\r\n\x00") {
		return "", fmt.Errorf("Docker reported an invalid data root %q", actual)
	}
	if strings.TrimSpace(requested) != "" && filepath.Clean(requested) != actual {
		return "", fmt.Errorf("Docker data root changed from %s to %s; use an explicit repair workflow", requested, actual)
	}
	return actual, nil
}

func validateUniqueNumericGroups(ctx context.Context, names ...string) error {
	groupOutput, err := exec.CommandContext(ctx, "getent", "group").Output()
	if err != nil {
		return fmt.Errorf("inspect system groups: %w", err)
	}
	return validateUniqueNumericGroupRecords(strings.Split(strings.TrimSpace(string(groupOutput)), "\n"), names...)
}

func validateUniqueNumericGroupRecords(records []string, names ...string) error {
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}
	ids := make(map[string]string, len(names))
	for _, line := range records {
		fields := strings.Split(line, ":")
		if len(fields) < 3 {
			continue
		}
		if _, ok := wanted[fields[0]]; ok {
			if fields[2] == "0" {
				return fmt.Errorf("platform group %s must not use GID 0", fields[0])
			}
			ids[fields[0]] = fields[2]
		}
	}
	for _, name := range names {
		gid, ok := ids[name]
		if !ok {
			return fmt.Errorf("platform group %s was not found", name)
		}
		for _, line := range records {
			fields := strings.Split(line, ":")
			if len(fields) >= 3 && fields[2] == gid && fields[0] != name {
				return fmt.Errorf("platform group %s shares numeric GID %s with %s", name, gid, fields[0])
			}
		}
	}
	return nil
}

func validateDedicatedWorkerGroup(ctx context.Context, group string, workerUser string) error {
	groupOutput, err := exec.CommandContext(ctx, "getent", "group", group).Output()
	if err != nil {
		return fmt.Errorf("inspect platform worker group: %w", err)
	}
	passwdOutput, err := exec.CommandContext(ctx, "getent", "passwd").Output()
	if err != nil {
		return fmt.Errorf("inspect system accounts for platform worker group: %w", err)
	}
	return validateDedicatedWorkerGroupRecords(group, workerUser, strings.Split(strings.TrimSpace(string(groupOutput)), ":"), strings.Split(strings.TrimSpace(string(passwdOutput)), "\n"))
}

func validateDedicatedWorkerGroupRecords(group string, workerUser string, groupFields []string, passwdRecords []string) error {
	if len(groupFields) < 4 || groupFields[2] == "0" {
		return fmt.Errorf("platform worker group record is invalid or privileged")
	}
	for _, member := range strings.Split(groupFields[3], ",") {
		if member != "" && member != workerUser {
			return fmt.Errorf("platform worker group %s grants access to unexpected user %s", group, member)
		}
	}
	for _, line := range passwdRecords {
		fields := strings.Split(line, ":")
		if len(fields) >= 4 && fields[3] == groupFields[2] && fields[0] != workerUser {
			return fmt.Errorf("platform worker group %s is the primary group of unexpected user %s", group, fields[0])
		}
	}
	return nil
}

func (LocalHost) StageBinary(_ context.Context, source string, destination string) error {
	return stagePlatformBinary(source, destination)
}

func (LocalHost) InstallUnit(_ context.Context, name string, content string) error {
	if name != TakodUnitName && name != WorkerUnitName {
		return fmt.Errorf("unsupported platform unit %q", name)
	}
	path := filepath.Join("/etc/systemd/system", name)
	if err := fileutil.WriteFileAtomic(path, []byte(content), 0644); err != nil {
		return err
	}
	return os.Chown(path, 0, 0)
}

func (LocalHost) ReloadEnableRestart(ctx context.Context, names ...string) error {
	if err := runLocalCommand(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	for _, name := range names {
		if name != TakodUnitName && name != WorkerUnitName {
			return fmt.Errorf("unsupported platform unit %q", name)
		}
		if err := runLocalCommand(ctx, "systemctl", "enable", name); err != nil {
			return err
		}
		if err := runLocalCommand(ctx, "systemctl", "restart", name); err != nil {
			return err
		}
	}
	return nil
}

func (LocalHost) WaitReady(ctx context.Context, check ReadinessCheck) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	journalPath := filepath.Join(check.StateDir, DefaultJournalName)
	var lastErr error
	for {
		if err := runLocalCommand(ctx, "systemctl", "is-active", "--quiet", TakodUnitName); err != nil {
			lastErr = err
		} else if err := runLocalCommand(ctx, "systemctl", "is-active", "--quiet", WorkerUnitName); err != nil {
			lastErr = err
		} else {
			agent, err := takodclient.NewLocalAgentClient(check.SocketPath)
			if err == nil {
				status, statusErr := agent.Status(ctx)
				agent.CloseIdleConnections()
				if statusErr == nil && status != nil && status.Identity != nil && status.Identity.Matches(check.ClusterID, check.NodeID) {
					if ready, readyErr := hasWorkerReadyRecord(journalPath, check.NodeID, check.Since); readyErr == nil && ready {
						return nil
					} else if readyErr != nil {
						lastErr = readyErr
					} else {
						lastErr = fmt.Errorf("worker readiness record has not been observed")
					}
				} else if statusErr != nil {
					lastErr = statusErr
				} else {
					lastErr = fmt.Errorf("takod reported the wrong installation identity")
				}
			} else {
				lastErr = err
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("platform services did not become ready: %w (last check: %v)", ctx.Err(), lastErr)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func hasWorkerReadyRecord(path string, nodeID string, since time.Time) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	const readinessTailBytes int64 = 2 * maxJournalFrameBytes
	offset := info.Size() - readinessTailBytes
	if offset < 0 {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return false, err
	}
	data, err := io.ReadAll(io.LimitReader(file, readinessTailBytes))
	if err != nil {
		return false, err
	}
	if offset > 0 {
		if boundary := strings.IndexByte(string(data), '\n'); boundary >= 0 {
			data = data[boundary+1:]
		} else {
			data = nil
		}
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if boundary := strings.LastIndexByte(string(data), '\n'); boundary >= 0 {
			data = data[:boundary+1]
		} else {
			data = nil
		}
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 64<<10), int(maxJournalFrameBytes))
	for scanner.Scan() {
		var record OperationRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return false, fmt.Errorf("decode platform operation journal: %w", err)
		}
		if record.Operation == "platform.worker" && record.Phase == "ready" && record.Status == "completed" && record.NodeID == nodeID && !record.Timestamp.Before(since) {
			return true, nil
		}
	}
	return false, scanner.Err()
}

func ensureSystemGroup(ctx context.Context, name string) error {
	if err := runLocalCommand(ctx, "getent", "group", name); err == nil {
		return nil
	}
	if err := runLocalCommand(ctx, "groupadd", "--system", name); err != nil {
		return fmt.Errorf("create system group %s: %w", name, err)
	}
	return nil
}

func localNumericID(ctx context.Context, flag string, user string) (int, error) {
	output, err := exec.CommandContext(ctx, "id", flag, user).Output()
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 0, err
	}
	return value, nil
}

func localGroupNumericID(ctx context.Context, group string) (int, error) {
	output, err := exec.CommandContext(ctx, "getent", "group", group).Output()
	if err != nil {
		return 0, err
	}
	fields := strings.Split(strings.TrimSpace(string(output)), ":")
	if len(fields) < 3 {
		return 0, fmt.Errorf("group %s has an invalid record", group)
	}
	value, err := strconv.Atoi(fields[2])
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("group %s must have a non-root numeric GID", group)
	}
	return value, nil
}

func localNumericGroups(ctx context.Context, user string) ([]int, error) {
	output, err := exec.CommandContext(ctx, "id", "-G", user).Output()
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(output))
	groups := make([]int, 0, len(fields))
	for _, field := range fields {
		gid, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("worker account returned invalid supplementary GID %q", field)
		}
		groups = append(groups, gid)
	}
	return groups, nil
}

func validateWorkerNumericGroups(groups []int, workerGID int, socketGID int) error {
	allowed := map[int]bool{workerGID: true, socketGID: true}
	seen := make(map[int]bool, len(groups))
	for _, gid := range groups {
		if gid <= 0 || !allowed[gid] {
			return fmt.Errorf("platform worker belongs to unexpected or privileged supplementary GID %d", gid)
		}
		seen[gid] = true
	}
	if !seen[workerGID] || !seen[socketGID] {
		return fmt.Errorf("platform worker must belong to only its dedicated and socket groups")
	}
	return nil
}

func validateWorkerNumericGroupsBeforeGrant(groups []int, workerGID int, socketGID int) error {
	allowed := map[int]bool{workerGID: true, socketGID: true}
	seenWorker := false
	for _, gid := range groups {
		if gid <= 0 || !allowed[gid] {
			return fmt.Errorf("platform worker belongs to unexpected or privileged supplementary GID %d", gid)
		}
		if gid == workerGID {
			seenWorker = true
		}
	}
	if !seenWorker {
		return fmt.Errorf("platform worker is missing its dedicated primary group")
	}
	return nil
}

func validateExistingPlatformUser(ctx context.Context, user string, group string) error {
	output, err := exec.CommandContext(ctx, "getent", "passwd", user).Output()
	if err != nil {
		return fmt.Errorf("inspect platform worker user: %w", err)
	}
	fields := strings.Split(strings.TrimSpace(string(output)), ":")
	if len(fields) != 7 {
		return fmt.Errorf("platform worker user %s has an invalid passwd record", user)
	}
	uid, err := strconv.Atoi(fields[2])
	if err != nil || uid <= 0 || uid >= 1000 {
		return fmt.Errorf("platform worker user %s must be a non-root system account", user)
	}
	groupOutput, err := exec.CommandContext(ctx, "getent", "group", group).Output()
	if err != nil {
		return fmt.Errorf("inspect platform worker group: %w", err)
	}
	groupFields := strings.Split(strings.TrimSpace(string(groupOutput)), ":")
	return validatePlatformAccountFields(user, group, fields, groupFields)
}

func validatePlatformAccountFields(user string, group string, fields []string, groupFields []string) error {
	if len(fields) != 7 || len(groupFields) < 3 {
		return fmt.Errorf("platform worker account records are invalid")
	}
	uid, err := strconv.Atoi(fields[2])
	if err != nil || uid <= 0 || uid >= 1000 {
		return fmt.Errorf("platform worker user %s must be a non-root system account", user)
	}
	if fields[3] != groupFields[2] {
		return fmt.Errorf("platform worker user %s must have %s as its primary group", user, group)
	}
	if fields[5] != DefaultStateDir {
		return fmt.Errorf("platform worker user %s must use %s as its home directory", user, DefaultStateDir)
	}
	if fields[6] != "/usr/sbin/nologin" && fields[6] != "/sbin/nologin" && fields[6] != "/bin/false" {
		return fmt.Errorf("platform worker user %s must use a non-login shell", user)
	}
	return nil
}

func runLocalCommand(ctx context.Context, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s failed: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}
