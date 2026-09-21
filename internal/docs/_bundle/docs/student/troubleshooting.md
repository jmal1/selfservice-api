# Troubleshooting

Use these checks for common lab access problems before requesting help.

## Sign-in problems

1. Start again from the Crucible sign-in page.
2. Complete the Authentik multi-factor prompt and confirm your device clock is accurate.
3. If the prompt loops, try a private browser window or clear the site's sign-in cookies.

**Expected result:** You return to **My Labs** with your account visible.

## Forgot password or locked out of Authentik

Talk to your instructor. They can create a one-time recovery link (or set a temporary password) in Authentik. Do not email passwords in plain text, and never share multi-factor codes. After you reset, sign in again from the Crucible sign-in page.

**Expected result:** You receive a recovery link from your instructor, set a new password, and return to **My Labs**.

## Provisioning problems

1. Open **My Labs** and refresh the lab status after a few minutes.
2. Read any message shown with the deployment status.
3. Do not delete the lab unless you want a fresh lab and are willing to lose its current work.

**Expected result:** The lab becomes active, or you have a status message to share with support.

## Browser console problems

1. Confirm the VM is running, then reopen the console from that VM's card.
2. Refresh the browser and try again.
3. If the console opens but sign-in fails, use only the username and password displayed for that VM.

**Expected result:** You can reach the VM's login screen in the browser.

## VPN, SSH, and RDP problems

1. Confirm NetBird is signed in and connected.
2. Use only the IP address shown for your own VM in Crucible.
3. Confirm the VM is running.
4. For SSH, make sure the Linux SSH service is running and permitted by the guest firewall. For RDP, make sure Remote Desktop is enabled and permitted by the Windows guest firewall.
5. Use the browser console to correct the guest service if needed.

**Expected result:** Your connection reaches your own VM. A connected VPN alone does not guarantee that SSH or RDP is enabled in the guest.

## Request help

Include your Crucible username, lab and VM name, the VM IP displayed for your VM, the time and time zone, the action you took, the exact visible error, and a screenshot if possible. Never include your password, multi-factor code, or other secret in a support request.
