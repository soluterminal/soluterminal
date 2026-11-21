//! Private no-clobber publication. Production migration callers remain disabled
//! until the Windows fixture proves NTFS, flags, DACL and refusal on replacement.

use std::{
    fs,
    io::{self, Write},
    path::{Path, PathBuf},
    sync::atomic::{AtomicU64, Ordering},
};

const MAX_BYTES: usize = 16 * 1024;
const WRITE_THROUGH: u32 = 0x8; // Deliberately no REPLACE_EXISTING.

pub(crate) struct Directory {
    root: PathBuf,
}

impl Directory {
    pub(crate) fn open(root: &Path) -> io::Result<Self> {
        // Never create or infer a profile directory. Callers bind an existing
        // explicit root; canonicalizing also resolves any parent junctions.
        let root = fs::canonicalize(root)?;
        if !fs::metadata(&root)?.is_dir() {
            return Err(refusal());
        }
        qualify_volume(&root)?;
        Ok(Self { root })
    }

    pub(crate) fn publish(&self, name: &str, data: &[u8]) -> io::Result<()> {
        if !matches!(
            name,
            "dual-migration.json"
                | "dual-migration-sending.json"
                | "dual-migration-confirmed.json"
                | "dual-retired.json"
        ) || data.len() > MAX_BYTES
        {
            return Err(refusal());
        }
        static SEQ: AtomicU64 = AtomicU64::new(0);
        let temp = self.root.join(format!(
            ".migration-private-{}-{}.tmp",
            std::process::id(),
            SEQ.fetch_add(1, Ordering::Relaxed)
        ));
        commit(
            &mut FilePublication {
                temp,
                target: self.root.join(name),
                file: None,
                created: false,
            },
            data,
        )
    }
}

// The action's ACK is returned only after every step. The fake used by tests
// models volatile writes and durable publication separately, including crashes.
trait Publication {
    fn create(&mut self) -> io::Result<()>;
    fn write(&mut self, bytes: &[u8]) -> io::Result<()>;
    fn sync(&mut self) -> io::Result<()>;
    fn close(&mut self) -> io::Result<()>;
    fn publish_durable(&mut self) -> io::Result<()>;
}
fn commit(store: &mut impl Publication, bytes: &[u8]) -> io::Result<()> {
    store.create()?;
    store.write(bytes)?;
    store.sync()?;
    store.close()?;
    store.publish_durable()
}
struct FilePublication {
    temp: PathBuf,
    target: PathBuf,
    file: Option<fs::File>,
    created: bool,
}
impl Publication for FilePublication {
    fn create(&mut self) -> io::Result<()> {
        self.file = Some(create_private(&self.temp)?);
        self.created = true;
        Ok(())
    }
    fn write(&mut self, bytes: &[u8]) -> io::Result<()> {
        self.file.as_mut().ok_or_else(refusal)?.write_all(bytes)
    }
    fn sync(&mut self) -> io::Result<()> {
        self.file.as_ref().ok_or_else(refusal)?.sync_all()
    }
    fn close(&mut self) -> io::Result<()> {
        drop(self.file.take().ok_or_else(refusal)?);
        Ok(())
    }
    fn publish_durable(&mut self) -> io::Result<()> {
        let result = publish_new(&self.temp, &self.target);
        #[cfg(windows)]
        if result.is_ok() {
            // MoveFileExW consumed this path. Do not remove an unrelated file
            // another process might subsequently create with the same name.
            self.created = false;
        }
        result
    }
}
impl Drop for FilePublication {
    fn drop(&mut self) {
        drop(self.file.take());
        // No adoption or removal on CREATE_NEW collision; only our own temp.
        if self.created {
            let _ = fs::remove_file(&self.temp);
        }
    }
}

fn refusal() -> io::Error {
    io::Error::new(
        io::ErrorKind::PermissionDenied,
        "migration storage requirements are not satisfied",
    )
}

#[cfg(not(windows))]
fn qualify_volume(_: &Path) -> io::Result<()> {
    Ok(())
} // Fixture adapter only.
#[cfg(unix)]
fn create_private(path: &Path) -> io::Result<fs::File> {
    use std::os::unix::fs::OpenOptionsExt;
    fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(path)
}
#[cfg(not(windows))]
fn publish_new(from: &Path, to: &Path) -> io::Result<()> {
    fs::hard_link(from, to)?;
    fs::File::open(to.parent().ok_or_else(refusal)?)?.sync_all()
}

#[cfg(windows)]
use windows::{create_private, publish_new, qualify_volume};

#[cfg(windows)]
mod windows {
    use super::*;
    use std::{
        ffi::c_void,
        os::windows::{
            ffi::OsStrExt,
            io::{AsRawHandle, FromRawHandle},
        },
        ptr::null_mut,
    };
    type Handle = *mut c_void;
    #[repr(C)]
    struct SecurityAttributes {
        length: u32,
        descriptor: *mut c_void,
        inherit: i32,
    }
    #[repr(C)]
    struct TokenUser {
        sid: *mut c_void,
        attributes: u32,
    }
    #[link(name = "kernel32")]
    extern "system" {
        fn GetCurrentProcess() -> Handle;
        fn CloseHandle(handle: Handle) -> i32;
        fn LocalFree(memory: *mut c_void) -> *mut c_void;
        fn GetVolumePathNameW(path: *const u16, volume: *mut u16, size: u32) -> i32;
        fn GetDriveTypeW(root: *const u16) -> u32;
        fn GetVolumeInformationW(
            root: *const u16,
            name: *mut u16,
            name_size: u32,
            serial: *mut u32,
            max_component: *mut u32,
            flags: *mut u32,
            filesystem: *mut u16,
            filesystem_size: u32,
        ) -> i32;
        fn CreateFileW(
            path: *const u16,
            access: u32,
            share: u32,
            security: *const SecurityAttributes,
            creation: u32,
            flags: u32,
            template: Handle,
        ) -> Handle;
        fn MoveFileExW(from: *const u16, to: *const u16, flags: u32) -> i32;
    }
    #[link(name = "advapi32")]
    extern "system" {
        fn OpenProcessToken(process: Handle, access: u32, token: *mut Handle) -> i32;
        fn GetTokenInformation(
            token: Handle,
            class: u32,
            buffer: *mut c_void,
            length: u32,
            needed: *mut u32,
        ) -> i32;
        fn ConvertSidToStringSidW(sid: *const c_void, text: *mut *mut u16) -> i32;
        fn GetSecurityInfo(
            handle: Handle,
            kind: u32,
            information: u32,
            owner: *mut *mut c_void,
            group: *mut *mut c_void,
            dacl: *mut *mut c_void,
            sacl: *mut *mut c_void,
            descriptor: *mut *mut c_void,
        ) -> u32;
        fn GetSecurityDescriptorControl(
            descriptor: *const c_void,
            control: *mut u16,
            revision: *mut u32,
        ) -> i32;
        fn GetAclInformation(
            acl: *const c_void,
            information: *mut c_void,
            size: u32,
            class: u32,
        ) -> i32;
        fn GetAce(acl: *const c_void, index: u32, ace: *mut *mut c_void) -> i32;
        fn EqualSid(first: *const c_void, second: *const c_void) -> i32;
        fn ConvertStringSecurityDescriptorToSecurityDescriptorW(
            text: *const u16,
            revision: u32,
            descriptor: *mut *mut c_void,
            size: *mut u32,
        ) -> i32;
    }
    struct OwnedHandle(Handle);
    impl Drop for OwnedHandle {
        fn drop(&mut self) {
            unsafe {
                CloseHandle(self.0);
            }
        }
    }
    struct LocalMemory(*mut c_void);
    impl Drop for LocalMemory {
        fn drop(&mut self) {
            unsafe {
                LocalFree(self.0);
            }
        }
    }
    fn wide(path: &Path) -> io::Result<Vec<u16>> {
        let mut result: Vec<_> = path.as_os_str().encode_wide().collect();
        if result.contains(&0) {
            return Err(refusal());
        }
        result.push(0);
        Ok(result)
    }
    fn token_user() -> io::Result<Vec<usize>> {
        let mut handle = null_mut();
        if unsafe { OpenProcessToken(GetCurrentProcess(), 0x8, &mut handle) } == 0 {
            return Err(io::Error::last_os_error());
        }
        let token = OwnedHandle(handle);
        let mut size = 0;
        unsafe {
            GetTokenInformation(token.0, 1, null_mut(), 0, &mut size);
        } // TokenUser, NEVER TokenOwner.
        if size == 0 || size > 65536 {
            return Err(refusal());
        }
        let mut bytes = vec![
            0usize;
            (size as usize + std::mem::size_of::<usize>() - 1)
                / std::mem::size_of::<usize>()
        ];
        if unsafe { GetTokenInformation(token.0, 1, bytes.as_mut_ptr().cast(), size, &mut size) }
            == 0
        {
            return Err(io::Error::last_os_error());
        }
        Ok(bytes)
    }
    pub(super) fn qualify_volume(root: &Path) -> io::Result<()> {
        let mut volume = vec![0u16; 32768];
        let root = wide(root)?;
        if unsafe { GetVolumePathNameW(root.as_ptr(), volume.as_mut_ptr(), volume.len() as u32) }
            == 0
        {
            return Err(refusal());
        }
        if unsafe { GetDriveTypeW(volume.as_ptr()) } != 3 {
            return Err(refusal());
        } // DRIVE_FIXED only.
        let mut filesystem = [0u16; 32];
        if unsafe {
            GetVolumeInformationW(
                volume.as_ptr(),
                null_mut(),
                0,
                null_mut(),
                null_mut(),
                null_mut(),
                filesystem.as_mut_ptr(),
                filesystem.len() as u32,
            )
        } == 0
        {
            return Err(refusal());
        }
        let length = filesystem
            .iter()
            .position(|c| *c == 0)
            .ok_or_else(refusal)?;
        if !String::from_utf16_lossy(&filesystem[..length]).eq_ignore_ascii_case("NTFS") {
            return Err(refusal());
        }
        Ok(())
    }
    pub(super) fn create_private(path: &Path) -> io::Result<fs::File> {
        create_private_checked(path, verify_private_file)
    }
    fn create_private_checked(
        path: &Path,
        verify: impl FnOnce(&fs::File) -> io::Result<()>,
    ) -> io::Result<fs::File> {
        let token = token_user()?;
        let user = unsafe { &*(token.as_ptr().cast::<TokenUser>()) };
        let mut text = null_mut();
        if unsafe { ConvertSidToStringSidW(user.sid, &mut text) } == 0 {
            return Err(io::Error::last_os_error());
        }
        let _sid_text = LocalMemory(text.cast());
        let mut len = 0;
        while len < 184 && unsafe { *text.add(len) } != 0 {
            len += 1;
        }
        if len == 184 {
            return Err(refusal());
        }
        let sid = String::from_utf16(unsafe { std::slice::from_raw_parts(text, len) })
            .map_err(|_| refusal())?;
        // Owner is the token USER even when an elevated token's TokenOwner is
        // Administrators. Protected DACL, one user ACE, no SYSTEM/inherited ACEs.
        let descriptor: Vec<u16> = format!("O:{sid}D:P(A;;FA;;;{sid})")
            .encode_utf16()
            .chain(Some(0))
            .collect();
        let mut sd = null_mut();
        if unsafe {
            ConvertStringSecurityDescriptorToSecurityDescriptorW(
                descriptor.as_ptr(),
                1,
                &mut sd,
                null_mut(),
            )
        } == 0
        {
            return Err(io::Error::last_os_error());
        }
        let _sd = LocalMemory(sd);
        let security = SecurityAttributes {
            length: std::mem::size_of::<SecurityAttributes>() as u32,
            descriptor: sd,
            inherit: 0,
        };
        let original_path = path;
        let path = wide(path)?;
        let handle =
            unsafe { CreateFileW(path.as_ptr(), 0xC0000000, 0, &security, 1, 0x80, null_mut()) }; // RW, CREATE_NEW.
        if handle as isize == -1 {
            return Err(io::Error::last_os_error());
        }
        let file = unsafe { fs::File::from_raw_handle(handle) };
        if let Err(error) = verify(&file) {
            drop(file);
            // This is only the fresh CREATE_NEW temporary, never a final marker.
            let _ = fs::remove_file(original_path);
            return Err(error);
        }
        Ok(file)
    }
    #[cfg(test)]
    pub(super) fn reject_private_for_fixture(path: &Path) -> io::Result<fs::File> {
        create_private_checked(path, |_| Err(refusal()))
    }

    pub(super) fn verify_private_file(file: &fs::File) -> io::Result<()> {
        let mut owner = null_mut();
        let mut dacl = null_mut();
        let mut sd = null_mut();
        let status = unsafe {
            GetSecurityInfo(
                file.as_raw_handle(),
                1,
                0x5,
                &mut owner,
                null_mut(),
                &mut dacl,
                null_mut(),
                &mut sd,
            )
        };
        if status != 0 {
            return Err(refusal());
        }
        let _sd = LocalMemory(sd);
        if sd.is_null() || owner.is_null() || dacl.is_null() {
            return Err(refusal());
        }
        let mut control = 0;
        let mut revision = 0;
        if unsafe { GetSecurityDescriptorControl(sd, &mut control, &mut revision) } == 0
            || control & 0x1000 == 0
        {
            return Err(refusal());
        }
        #[repr(C)]
        struct AclSize {
            count: u32,
            used: u32,
            free: u32,
        }
        let mut size = AclSize {
            count: 0,
            used: 0,
            free: 0,
        };
        if unsafe {
            GetAclInformation(
                dacl,
                (&mut size as *mut AclSize).cast(),
                std::mem::size_of::<AclSize>() as u32,
                2,
            )
        } == 0
            || size.count != 1
        {
            return Err(refusal());
        }
        let mut ace = null_mut();
        if unsafe { GetAce(dacl, 0, &mut ace) } == 0 || ace.is_null() {
            return Err(refusal());
        }
        #[repr(C)]
        struct AllowedAce {
            kind: u8,
            flags: u8,
            size: u16,
            mask: u32,
            sid_start: u32,
        }
        let allowed = unsafe { &*(ace.cast::<AllowedAce>()) };
        if allowed.kind != 0 || allowed.flags != 0 || allowed.size < 16 || allowed.mask != 0x1f01ff
        {
            return Err(refusal());
        }
        let token = token_user()?;
        let user = unsafe { &*(token.as_ptr().cast::<TokenUser>()) };
        let sid = std::ptr::addr_of!(allowed.sid_start).cast::<c_void>();
        if unsafe { EqualSid(user.sid, owner) } == 0 || unsafe { EqualSid(user.sid, sid) } == 0 {
            return Err(refusal());
        }
        Ok(())
    }

    pub(super) fn publish_new(from: &Path, to: &Path) -> io::Result<()> {
        if from.parent() != to.parent() {
            return Err(refusal());
        }
        let from = wide(from)?;
        let to = wide(to)?;
        if unsafe { MoveFileExW(from.as_ptr(), to.as_ptr(), WRITE_THROUGH) } == 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(())
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    struct Fixture(PathBuf);
    impl Fixture {
        fn new() -> Self {
            static SEQ: AtomicU64 = AtomicU64::new(0);
            let path = std::env::temp_dir().join(format!(
                "solu-private-intent-fixture-{}-{}",
                std::process::id(),
                SEQ.fetch_add(1, Ordering::Relaxed)
            ));
            fs::create_dir(&path).unwrap();
            Self(path)
        }
    }
    impl Drop for Fixture {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }
    #[test]
    fn publication_is_private_and_never_replaces_a_record() {
        let f = Fixture::new();
        let store = Directory::open(&f.0).unwrap();
        for name in [
            "dual-migration.json",
            "dual-migration-sending.json",
            "dual-migration-confirmed.json",
            "dual-retired.json",
        ] {
            store.publish(name, b"private fixture").unwrap();
            assert!(store.publish(name, b"replacement").is_err());
            assert_eq!(fs::read(f.0.join(name)).unwrap(), b"private fixture");
            #[cfg(windows)]
            windows::verify_private_file(&fs::File::open(f.0.join(name)).unwrap()).unwrap();
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                assert_eq!(
                    fs::metadata(f.0.join(name)).unwrap().permissions().mode() & 0o777,
                    0o600
                );
            }
        }
        assert_eq!(fs::read_dir(&f.0).unwrap().count(), 4);
    }
    #[test]
    fn refuse_unbound_names_and_oversized_content_before_creation() {
        let f = Fixture::new();
        let store = Directory::open(&f.0).unwrap();
        assert!(store.publish("../elsewhere", b"").is_err());
        assert!(store
            .publish("dual-migration.json", &vec![0; MAX_BYTES + 1])
            .is_err());
        assert_eq!(fs::read_dir(&f.0).unwrap().count(), 0);
        assert_eq!(WRITE_THROUGH, 0x8);
        assert_eq!(WRITE_THROUGH & 0x1, 0);
    }
    #[test]
    fn crash_between_every_durability_step_never_acknowledges_an_undurable_action() {
        struct PowerLoss {
            fail_after: usize,
            step: usize,
            volatile: Vec<u8>,
            flushed: Vec<u8>,
            durable: Option<Vec<u8>>,
            closed: bool,
        }
        impl PowerLoss {
            fn checkpoint(&mut self) -> io::Result<()> {
                self.step += 1;
                if self.step == self.fail_after {
                    return Err(io::Error::new(
                        io::ErrorKind::Interrupted,
                        "fixture power loss",
                    ));
                }
                Ok(())
            }
        }
        impl Publication for PowerLoss {
            fn create(&mut self) -> io::Result<()> {
                self.checkpoint()
            }
            fn write(&mut self, bytes: &[u8]) -> io::Result<()> {
                self.volatile = bytes.to_vec();
                self.checkpoint()
            }
            fn sync(&mut self) -> io::Result<()> {
                self.flushed = self.volatile.clone();
                self.checkpoint()
            }
            fn close(&mut self) -> io::Result<()> {
                self.closed = true;
                self.checkpoint()
            }
            fn publish_durable(&mut self) -> io::Result<()> {
                assert!(self.closed);
                self.durable = Some(self.flushed.clone());
                self.checkpoint()
            }
        }
        for fail_after in 1..=6 {
            let mut store = PowerLoss {
                fail_after,
                step: 0,
                volatile: vec![],
                flushed: vec![],
                durable: None,
                closed: false,
            };
            let result = commit(&mut store, b"bound fixture intent");
            // After a simulated crash only durable state survives. A crash
            // after publication but before ACK may leave a fence, never an ACK.
            store.volatile.clear();
            store.flushed.clear();
            if result.is_ok() {
                assert_eq!(
                    store.durable.as_deref(),
                    Some(b"bound fixture intent".as_slice())
                );
                assert_eq!(fail_after, 6);
            } else {
                assert_eq!(store.step, fail_after);
            }
            if fail_after < 5 {
                assert!(store.durable.is_none());
            }
        }
    }
    #[cfg(windows)]
    #[test]
    fn failed_acl_check_removes_only_its_fresh_temporary() {
        let f = Fixture::new();
        let path = f.0.join("private.tmp");
        assert!(windows::reject_private_for_fixture(&path).is_err());
        assert!(!path.exists());
        fs::write(&path, b"pre-existing fixture").unwrap();
        assert!(windows::reject_private_for_fixture(&path).is_err());
        assert_eq!(fs::read(&path).unwrap(), b"pre-existing fixture");
    }

    #[test]
    fn interrupted_real_publication_cleans_only_its_own_temporary() {
        struct Failing<'a> {
            inner: &'a mut FilePublication,
            stage: usize,
            fail: usize,
        }
        impl Failing<'_> {
            fn checkpoint(&mut self) -> io::Result<()> {
                self.stage += 1;
                if self.stage == self.fail {
                    Err(io::Error::new(
                        io::ErrorKind::Interrupted,
                        "fixture failure",
                    ))
                } else {
                    Ok(())
                }
            }
        }
        impl Publication for Failing<'_> {
            fn create(&mut self) -> io::Result<()> {
                self.inner.create()?;
                self.checkpoint()
            }
            fn write(&mut self, bytes: &[u8]) -> io::Result<()> {
                self.inner.write(bytes)?;
                self.checkpoint()
            }
            fn sync(&mut self) -> io::Result<()> {
                self.inner.sync()?;
                self.checkpoint()
            }
            fn close(&mut self) -> io::Result<()> {
                self.inner.close()?;
                self.checkpoint()
            }
            fn publish_durable(&mut self) -> io::Result<()> {
                self.inner.publish_durable()?;
                self.checkpoint()
            }
        }
        for fail in 1..=5 {
            let f = Fixture::new();
            qualify_volume(&f.0).unwrap();
            let target = f.0.join("dual-migration.json");
            let temp = f.0.join("own.tmp");
            {
                let mut inner = FilePublication {
                    temp: temp.clone(),
                    target: target.clone(),
                    file: None,
                    created: false,
                };
                let mut adapter = Failing {
                    inner: &mut inner,
                    stage: 0,
                    fail,
                };
                assert!(commit(&mut adapter, b"fixture intent").is_err());
            }
            assert!(!temp.exists());
            if fail == 5 {
                assert_eq!(fs::read(&target).unwrap(), b"fixture intent");
            } else {
                assert!(!target.exists());
            }
        }
    }
    #[cfg(windows)]
    #[test]
    fn moved_temporary_name_is_never_cleaned_as_if_still_owned() {
        let f = Fixture::new();
        qualify_volume(&f.0).unwrap();
        let temp = f.0.join("own.tmp");
        let target = f.0.join("dual-migration.json");
        let mut writer = FilePublication {
            temp: temp.clone(),
            target,
            file: None,
            created: false,
        };
        commit(&mut writer, b"private fixture").unwrap();
        fs::write(&temp, b"subsequent unrelated fixture").unwrap();
        drop(writer);
        assert_eq!(fs::read(&temp).unwrap(), b"subsequent unrelated fixture");
    }
}
