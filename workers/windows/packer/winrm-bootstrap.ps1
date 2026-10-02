<powershell>
# SPDX-License-Identifier: FSL-1.1-ALv2
# EC2 user data for Packer build instances only: WinRM over HTTPS (5986) with a throwaway self-signed
# certificate. The Administrator password is EC2's random one, retrieved by Packer with GetPasswordData and
# its temporary key pair (nothing secret is in this file). The finalize step removes the listener, the
# firewall rule and the certificate again before the image is captured.
$ErrorActionPreference = 'Stop'
Start-Transcript -Path 'C:\Windows\Temp\cucina-winrm-bootstrap.log' -Append | Out-Null
try {
  Set-Service -Name WinRM -StartupType Automatic
  Start-Service -Name WinRM
  Get-ChildItem -Path WSMan:\localhost\Listener | Remove-Item -Recurse -Force
  Get-ChildItem Cert:\LocalMachine\My | Where-Object { $_.FriendlyName -eq 'cucina-packer-winrm' } | Remove-Item -Force
  $cert = New-SelfSignedCertificate -CertStoreLocation Cert:\LocalMachine\My -DnsName 'packer' `
    -FriendlyName 'cucina-packer-winrm' -NotAfter (Get-Date).AddDays(3)
  New-Item -Path WSMan:\localhost\Listener -Transport HTTPS -Address * -CertificateThumbPrint $cert.Thumbprint -Force | Out-Null
  Set-Item -Path WSMan:\localhost\Service\Auth\Basic -Value $true
  Set-Item -Path WSMan:\localhost\Service\AllowUnencrypted -Value $false
  foreach ($setting in @(
      @{ Path = 'WSMan:\localhost\MaxTimeoutms'; Value = 7200000 },
      @{ Path = 'WSMan:\localhost\Shell\MaxMemoryPerShellMB'; Value = 8192 },
      @{ Path = 'WSMan:\localhost\Plugin\Microsoft.PowerShell\Quotas\MaxMemoryPerShellMB'; Value = 8192 })) {
    try { Set-Item -Path $setting.Path -Value $setting.Value } catch { Write-Output "warning: $($setting.Path): $_" }
  }
  Get-NetFirewallRule -Name 'cucina-packer-winrm-https' -ErrorAction SilentlyContinue | Remove-NetFirewallRule
  New-NetFirewallRule -Name 'cucina-packer-winrm-https' -DisplayName 'cucina packer WinRM HTTPS' -Direction Inbound `
    -Protocol TCP -LocalPort 5986 -Action Allow -Profile Any | Out-Null
  Restart-Service -Name WinRM
} finally {
  Stop-Transcript | Out-Null
}
</powershell>
