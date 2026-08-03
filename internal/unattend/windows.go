package unattend

// windows.go generates a Windows Setup autounattend.xml seed ISO.
//
// Windows Setup automatically detects an autounattend.xml at the ROOT of any
// removable media, so the seed ISO carries a single autounattend.xml at its
// root.
//
// SECURITY: the account passwords are NOT stored in plaintext. They use the
// Windows "encoded password" form: base64(UTF-16LE(password + elementName)),
// with <PlainText>false</PlainText>. This mirrors
// internal/provisioner/assets/windows-unattend.xml exactly. Storing plaintext
// here previously caused a real bug: sysprep scrubs plaintext <Password>
// elements on /generalize (replacing them with "*SENSITIVE*DATA*DELETED*"),
// which broke every downstream clone. The encoded form survives sysprep. See
// internal/provisioner/template_jobs.go (windowsUnattendGuestPath) for the
// full history.

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"html"
	"text/template"
	"unicode/utf16"
)

// encodeWindowsPassword returns the Windows unattend encoded-password value:
// the base64 of the UTF-16LE encoding of (password + elementName). The magic
// suffix is the name of the XML element the value lives in ("Password" for
// LocalAccount/AutoLogon <Password> elements, "AdministratorPassword" for an
// <AdministratorPassword> element). Sysprep does NOT scrub these.
func encodeWindowsPassword(password, elementName string) string {
	s := password + elementName
	units := utf16.Encode([]rune(s))
	b := make([]byte, len(units)*2)
	for i, u := range units {
		binary.LittleEndian.PutUint16(b[i*2:], u)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// ianaToWindowsTZ maps a few common IANA time zones to their Windows names.
// Windows unattend requires a Windows time-zone identifier, not an IANA one.
var ianaToWindowsTZ = map[string]string{
	"America/New_York":    "Eastern Standard Time",
	"America/Chicago":     "Central Standard Time",
	"America/Denver":      "Mountain Standard Time",
	"America/Los_Angeles": "Pacific Standard Time",
	"America/Phoenix":     "US Mountain Standard Time",
	"UTC":                 "UTC",
	"Etc/UTC":             "UTC",
}

func windowsTimeZone(iana string) string {
	if tz, ok := ianaToWindowsTZ[iana]; ok {
		return tz
	}
	return "Eastern Standard Time"
}

type autounattendData struct {
	ComputerName    string
	TimeZone        string
	Username        string
	EncodedPassword string
}

// buildAutounattendXML renders the autounattend.xml document for s.
func buildAutounattendXML(s Spec) (string, error) {
	computerName := s.Hostname
	if computerName == "" {
		computerName = "*"
	}

	data := autounattendData{
		ComputerName:    html.EscapeString(computerName),
		TimeZone:        html.EscapeString(windowsTimeZone(s.TimeZone)),
		Username:        html.EscapeString(s.Username),
		EncodedPassword: encodeWindowsPassword(s.Password, "Password"),
	}

	var buf bytes.Buffer
	if err := autounattendTemplate.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("unattend: render autounattend.xml: %w", err)
	}
	return buf.String(), nil
}

// buildAutounattendISO renders the Windows seed ISO with autounattend.xml at
// its root.
func buildAutounattendISO(s Spec) (name string, data []byte, err error) {
	xml, err := buildAutounattendXML(s)
	if err != nil {
		return "", nil, err
	}
	files := []isoFile{
		{Path: "autounattend.xml", Data: []byte(xml)},
	}
	iso, err := buildISO(files, "AUTOUNATTEND")
	if err != nil {
		return "", nil, err
	}
	return "seed-autounattend.iso", iso, nil
}

// autounattendTemplate mirrors internal/provisioner/assets/windows-unattend.xml.
// The <Password> values use the encoded (PlainText=false) form so sysprep does
// not scrub them.
var autounattendTemplate = template.Must(template.New("autounattend").Parse(`<?xml version="1.0" encoding="utf-8"?>
<unattend xmlns="urn:schemas-microsoft-com:unattend">

  <settings pass="specialize">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64"
      publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS"
      xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <ComputerName>{{.ComputerName}}</ComputerName>
      <TimeZone>{{.TimeZone}}</TimeZone>
    </component>

    <component name="Microsoft-Windows-TerminalServices-LocalSessionManager" processorArchitecture="amd64"
      publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS"
      xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <fDenyTSConnections>false</fDenyTSConnections>
    </component>

    <component name="Networking-MPSSVC-Svc" processorArchitecture="amd64"
      publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS"
      xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <FirewallGroups>
        <FirewallGroup wcm:action="add" wcm:keyValue="RemoteDesktop">
          <Active>true</Active>
          <Group>Remote Desktop</Group>
          <Profile>all</Profile>
        </FirewallGroup>
      </FirewallGroups>
    </component>

    <component name="Microsoft-Windows-Deployment" processorArchitecture="amd64"
      publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS"
      xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <RunSynchronous>
        <RunSynchronousCommand wcm:action="add">
          <Order>1</Order>
          <Path>cmd.exe /c sc config cloudbase-init start= delayed-auto</Path>
          <Description>Enable cloudbase-init (FirstLogonCommands are skipped on Server SKUs)</Description>
          <WillReboot>Never</WillReboot>
        </RunSynchronousCommand>
      </RunSynchronous>
    </component>
  </settings>

  <settings pass="oobeSystem">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64"
      publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS"
      xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">

      <OOBE>
        <HideEULAPage>true</HideEULAPage>
        <HideLocalAccountScreen>true</HideLocalAccountScreen>
        <HideOEMRegistrationScreen>true</HideOEMRegistrationScreen>
        <HideOnlineAccountScreens>true</HideOnlineAccountScreens>
        <HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE>
        <ProtectYourPC>3</ProtectYourPC>
        <SkipMachineOOBE>true</SkipMachineOOBE>
        <SkipUserOOBE>true</SkipUserOOBE>
      </OOBE>

      <UserAccounts>
        <LocalAccounts>
          <LocalAccount wcm:action="add">
            <Name>{{.Username}}</Name>
            <Group>Administrators</Group>
            <Password>
              <Value>{{.EncodedPassword}}</Value>
              <PlainText>false</PlainText>
            </Password>
          </LocalAccount>
        </LocalAccounts>
      </UserAccounts>

      <AutoLogon>
        <Enabled>true</Enabled>
        <Username>{{.Username}}</Username>
        <Password>
          <Value>{{.EncodedPassword}}</Value>
          <PlainText>false</PlainText>
        </Password>
        <LogonCount>1</LogonCount>
      </AutoLogon>

      <FirstLogonCommands>
        <SynchronousCommand wcm:order="1">
          <CommandLine>cmd.exe /c msiexec /i %SystemDrive%\cloudbase-init\CloudbaseInitSetup.msi /qn /norestart</CommandLine>
          <Description>Stage cloudbase-init install</Description>
          <RequiresUserInput>false</RequiresUserInput>
        </SynchronousCommand>
        <SynchronousCommand wcm:order="2">
          <CommandLine>sc config cloudbase-init start= auto</CommandLine>
          <Description>Re-enable cloudbase-init auto-start</Description>
          <RequiresUserInput>false</RequiresUserInput>
        </SynchronousCommand>
        <SynchronousCommand wcm:order="3">
          <CommandLine>net start cloudbase-init</CommandLine>
          <Description>Start cloudbase-init to apply guestinfo userdata</Description>
          <RequiresUserInput>false</RequiresUserInput>
        </SynchronousCommand>
      </FirstLogonCommands>

    </component>

    <component name="Microsoft-Windows-International-Core" processorArchitecture="amd64"
      publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS"
      xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <InputLocale>en-US</InputLocale>
      <SystemLocale>en-US</SystemLocale>
      <UILanguage>en-US</UILanguage>
      <UserLocale>en-US</UserLocale>
    </component>
  </settings>

</unattend>
`))
