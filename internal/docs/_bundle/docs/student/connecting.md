# Connecting to Your VM

Connect only to the VM IP address and credentials displayed for **your own** lab in Crucible.

## Use the browser console

1. Open your lab in **My Labs** and select the VM.
2. Select the console option and wait for the browser console to open.
3. Sign in with the username and password shown for that VM.

**Expected result:** You see and can use that VM's desktop or terminal in the browser.

## Connect through NetBird

1. Install the NetBird desktop app from the [official download page](https://netbird.io/download/).
2. Open NetBird. If it asks for a custom or self-hosted server, enter `https://netbird.jmal.io`.
3. Select **Connect**. In the browser, authorize with your course Authentik account and complete multi-factor authentication.
4. Return to NetBird and confirm that it shows **Connected**.
5. In Crucible, copy the IP address shown for the VM you own. Do not use an IP address from another lab.

**Expected result:** Your device can reach your own VM address through the VPN.

If you cannot authorize or connect, ask your instructor for help before continuing.

## Connect to Linux with SSH

1. Confirm NetBird is connected and your Linux VM is running.
2. Copy that VM's displayed username and IP address from Crucible.
3. Run `ssh username@your-vm-ip`, replacing both values with the ones shown for your VM.
4. Enter the displayed password when prompted.

**Expected result:** You receive a shell on your own Linux VM.

## Connect to Windows with RDP

1. Confirm NetBird is connected and your Windows VM is running.
2. Open Remote Desktop Connection and enter the IP address shown for your Windows VM.
3. Sign in with the username and password shown in Crucible for that VM.

**Expected result:** You see the desktop of your own Windows VM.

SSH and RDP depend on the guest service being enabled and allowed by that VM's firewall. If a connection fails, use the browser console to check the guest service, then read [Troubleshooting](troubleshooting.md).
