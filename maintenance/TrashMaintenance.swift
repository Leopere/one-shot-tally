import Foundation
import Darwin

let retentionSeconds: TimeInterval = 7 * 24 * 60 * 60

struct MaintenanceError: Error, CustomStringConvertible {
    let description: String
    init(_ message: String) { description = message }
}

struct Identity: Equatable {
    let device: dev_t
    let inode: ino_t
    let kind: mode_t
    let birthSeconds: Int
    let birthNanos: Int

    init(_ info: stat) {
        device = info.st_dev
        inode = info.st_ino
        kind = info.st_mode & S_IFMT
        birthSeconds = info.st_birthtimespec.tv_sec
        birthNanos = info.st_birthtimespec.tv_nsec
    }
}

struct MaintenanceReport: Codable {
    var scanned = 0
    var eligible = 0
    var deleted = 0
    var removedEntries = 0
    var partiallyDeleted = 0
    var skipped = 0
    var errors = 0
    var dryRun: Bool
    var retentionDays = 7
    var checkedAt = Date()
}

func oldEnough(_ added: Date?, now: Date) -> Bool {
    guard let added else { return false }
    let timestamp = added.timeIntervalSince1970
    let age = now.timeIntervalSince(added)
    return timestamp.isFinite && timestamp > 0 && age.isFinite && age > retentionSeconds
}

func entryStat(_ parent: Int32, _ name: String) throws -> stat {
    var info = stat()
    guard fstatat(parent, name, &info, AT_SYMLINK_NOFOLLOW) == 0 else {
        throw MaintenanceError("Cannot inspect entry (errno \(errno))")
    }
    return info
}

func namesInDirectory(_ fd: Int32) throws -> [String] {
    let copy = dup(fd)
    guard copy >= 0 else { throw MaintenanceError("Cannot duplicate directory descriptor") }
    guard let directory = fdopendir(copy) else {
        close(copy)
        throw MaintenanceError("Cannot enumerate directory")
    }
    defer { closedir(directory) }
    var names: [String] = []
    errno = 0
    while let entry = readdir(directory) {
        var raw = entry.pointee.d_name
        let name: String? = withUnsafeBytes(of: &raw) { bytes in
            String(bytes: bytes.prefix { $0 != 0 }, encoding: .utf8)
        }
        guard let name else { throw MaintenanceError("An entry name is not valid UTF-8") }
        if name != "." && name != ".." { names.append(name) }
        errno = 0
    }
    guard errno == 0 else { throw MaintenanceError("Directory enumeration failed") }
    return names
}

// All mutation is anchored to open directory descriptors. Descendant links are
// unlinked as directory entries; their targets are never opened or traversed.
func eraseEntry(parent: Int32, name: String, expected: Identity, device: dev_t,
                stillInRoot: () throws -> Void, didRemove: () -> Void = {}) throws {
    try stillInRoot()
    let info = try entryStat(parent, name)
    guard Identity(info) == expected, info.st_dev == device else {
        throw MaintenanceError("Entry identity or filesystem changed")
    }
    if (info.st_mode & S_IFMT) == S_IFDIR {
        let child = openat(parent, name, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
        guard child >= 0 else { throw MaintenanceError("Cannot open child directory (errno \(errno))") }
        defer { close(child) }
        var opened = stat()
        guard fstat(child, &opened) == 0, Identity(opened) == expected else {
            throw MaintenanceError("Directory changed while opening")
        }
        for childName in try namesInDirectory(child) {
            let childInfo = try entryStat(child, childName)
            try eraseEntry(parent: child, name: childName, expected: Identity(childInfo),
                           device: device, stillInRoot: stillInRoot, didRemove: didRemove)
        }
        try stillInRoot()
        guard Identity(try entryStat(parent, name)) == expected else {
            throw MaintenanceError("Directory changed before removal")
        }
        guard unlinkat(parent, name, AT_REMOVEDIR) == 0 else {
            throw MaintenanceError("Cannot remove directory (errno \(errno))")
        }
        didRemove()
    } else {
        try stillInRoot()
        guard Identity(try entryStat(parent, name)) == expected else {
            throw MaintenanceError("Entry changed before removal")
        }
        guard unlinkat(parent, name, 0) == 0 else {
            throw MaintenanceError("Cannot remove entry (errno \(errno))")
        }
        didRemove()
    }
}

// The CLI always supplies the current user's exact home Trash. Parameters here
// let tests exercise ordinary fixture directories without accessing real Trash.
func maintain(root: URL, now: Date, dryRun: Bool,
              addedDate: (URL) throws -> Date? = {
                  try $0.resourceValues(forKeys: [.addedToDirectoryDateKey]).addedToDirectoryDate
              },
              erase: (Int32, String, Identity, dev_t, () throws -> Void, () -> Void) throws -> Void = {
                  try eraseEntry(parent: $0, name: $1, expected: $2, device: $3, stillInRoot: $4, didRemove: $5)
              }) throws -> MaintenanceReport {
    var report = MaintenanceReport(dryRun: dryRun)
    let fd = open(root.path, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
    if fd < 0 && errno == ENOENT { return report }
    guard fd >= 0 else { throw MaintenanceError("Cannot open maintenance root (errno \(errno))") }
    defer { close(fd) }
    var rootInfo = stat()
    guard fstat(fd, &rootInfo) == 0, rootInfo.st_uid == geteuid(),
          rootInfo.st_mode & 0o077 == 0 else {
        throw MaintenanceError("Maintenance root must be a private directory owned by this user")
    }
    let rootIdentity = Identity(rootInfo)
    for name in try namesInDirectory(fd) {
        // Finder metadata is not a trashed user item.
        if name == ".DS_Store" || name == ".localized" { continue }
        report.scanned += 1
        let removedBefore = report.removedEntries
        do {
            let before = try entryStat(fd, name)
            let kind = before.st_mode & S_IFMT
            // Foundation may follow top-level links for resource metadata.
            // Added-date metadata is documented as inconsistent for hard links.
            guard (kind == S_IFDIR || kind == S_IFREG),
                  !(kind == S_IFREG && before.st_nlink > 1),
                  before.st_dev == rootInfo.st_dev else {
                report.skipped += 1
                continue
            }
            let identity = Identity(before)
            let item = root.appendingPathComponent(name)
            guard oldEnough(try addedDate(item), now: now) else {
                report.skipped += 1
                continue
            }
            // Discard resource caches and re-check just before mutation.
            let freshItem = URL(fileURLWithPath: item.path)
            let acceptedDate = try addedDate(freshItem)
            guard Identity(try entryStat(fd, name)) == identity,
                  oldEnough(acceptedDate, now: now) else {
                report.skipped += 1
                continue
            }
            report.eligible += 1
            if dryRun { continue }
            let stillInRoot = {
                var currentRoot = stat()
                guard lstat(root.path, &currentRoot) == 0,
                      Identity(currentRoot) == rootIdentity,
                      Identity(try entryStat(fd, name)) == identity else {
                    throw MaintenanceError("Item was moved or replaced during cleanup")
                }
                let currentDate = try addedDate(URL(fileURLWithPath: item.path))
                guard currentDate == acceptedDate, oldEnough(currentDate, now: now) else {
                    throw MaintenanceError("Item's time in Trash changed during cleanup")
                }
            }
            try erase(fd, name, identity, rootInfo.st_dev, stillInRoot, { report.removedEntries += 1 })
            report.deleted += 1
        } catch {
            report.errors += 1
            if report.removedEntries > removedBefore { report.partiallyDeleted += 1 }
            // Avoid recording filenames or file contents in background logs.
            fputs("trash-maintenance: \(error)\n", stderr)
        }
    }
    return report
}
