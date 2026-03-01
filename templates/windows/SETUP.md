# Windows 11 Template Setup Guide

This guide documents how to prepare the Windows 11 template VM for unattended
provisioning with cloudbase-init and the Self-Service Portal.

## Prerequisites

- Access to vCenter Web Client
- Windows 11 template VM (currently in OOBE state)
- `unattend.xml` from this directory

## Step 1: Boot the Template VM

1. Open vCenter → Navigate to the W11 template VM
2. Power it on and open the Web Console
3. Complete OOBE manually (create a temporary admin account)

## Step 2: Install VMware Tools

If not already installed:
1. In vCenter, right-click the VM → Guest OS → Install VMware Tools
2. Run the installer inside the VM
3. Reboot when prompted

## Step 3: Install Cloudbase-Init

1. Download the MSI installer from https://cloudbase.it/cloudbase-init/#download
2. Run the installer:
   - **Username:** `Student`
   - **Serial port:** leave default
   - On the final page, **UNCHECK "Run Sysprep"** — we'll do that manually
   - **UNCHECK "Shutdown"**
3. Click Finish

## Step 4: Configure Cloudbase-Init

Edit `C:\Program Files\Cloudbase Solutions\Cloudbase-Init\conf\cloudbase-init.conf`:

```ini
[DEFAULT]
username=Student
groups=Administrators
inject_user_password=true
first_logon_behaviour=no
metadata_services=cloudbaseinit.metadata.services.vmwareguestinfoservice.VMwareGuestInfoService
plugins=cloudbaseinit.plugins.common.userdata.UserDataPlugin,cloudbaseinit.plugins.common.sethostname.SetHostNamePlugin,cloudbaseinit.plugins.common.localscripts.LocalScriptsPlugin
allow_reboot=false
stop_service_on_exit=false
check_latest_version=false
```

Edit `C:\Program Files\Cloudbase Solutions\Cloudbase-Init\conf\cloudbase-init-unattend.conf`:

```ini
[DEFAULT]
username=Student
groups=Administrators
inject_user_password=true
metadata_services=cloudbaseinit.metadata.services.vmwareguestinfoservice.VMwareGuestInfoService
plugins=cloudbaseinit.plugins.common.sethostname.SetHostNamePlugin
allow_reboot=false
stop_service_on_exit=false
check_latest_version=false
```

## Step 5: Copy Unattend.xml

Copy `unattend.xml` from this directory to `C:\Windows\Panther\unattend.xml` on the template VM.

You can also place it at `C:\unattend.xml` as a backup location.

## Step 6: Disable Cloudbase-Init Service (Pre-Sysprep)

The service should NOT run during sysprep — it will be re-enabled by the
unattend.xml FirstLogonCommands after the first boot:

```powershell
Set-Service cloudbase-init -StartupType Disabled
```

## Step 7: Clean Up

Before sysprepping:
1. Delete the temporary admin account you created in Step 1
2. Clear browser history, temp files
3. Run `cleanmgr /sageset:0` → check all → `cleanmgr /sagerun:0`

## Step 8: Sysprep

Run from an elevated command prompt:

```cmd
C:\Windows\System32\Sysprep\sysprep.exe /generalize /oobe /shutdown /unattend:C:\Windows\Panther\unattend.xml
```

**Wait for the VM to shut down completely.** Do not interrupt sysprep.

## Step 9: Create Linked-Clone Base Snapshot

In vCenter:
1. Right-click the VM → Snapshots → Take Snapshot
2. Name: `linked-clone-base`
3. Description: `Auto-created for linked clone provisioning`
4. Uncheck "Snapshot the virtual machine's memory"
5. Uncheck "Quiesce guest file system"

## Step 10: Verify

1. Deploy a test pod with a Windows VM through the portal
2. The provisioner should:
   - Clone the VM (linked clone from snapshot)
   - Inject guestinfo.userdata with a PowerShell script to set the Student password
   - Power on the VM
3. After boot:
   - OOBE should be skipped (unattend.xml)
   - Student account should auto-login once
   - Cloudbase-init should start, read guestinfo, and set the Student password
   - RDP should be enabled and accessible

## Troubleshooting

- **Cloudbase-init logs:** `C:\Program Files\Cloudbase Solutions\Cloudbase-Init\log\cloudbase-init.log`
- **Check guestinfo was injected:** In vCenter → VM → Configure → Advanced → look for `guestinfo.userdata`
- **OOBE still showing:** The unattend.xml wasn't found — verify it's at `C:\Windows\Panther\unattend.xml`
- **RDP not working:** Check Windows Firewall → `netsh advfirewall firewall show rule name="Remote Desktop*"`
- **Password not set:** Check cloudbase-init log for errors reading VMwareGuestInfoService
