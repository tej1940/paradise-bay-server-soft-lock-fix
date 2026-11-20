$ErrorActionPreference = "Stop"

Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing

$baseUrl = "http://127.0.0.1:3300"
$form = New-Object System.Windows.Forms.Form
$form.Text = "Paradise Bay - Market"
$form.StartPosition = "Manual"
$form.Size = New-Object System.Drawing.Size(390, 190)
$form.MinimumSize = $form.Size
$form.MaximumSize = $form.Size
$form.TopMost = $true
$form.FormBorderStyle = "FixedDialog"
$form.MaximizeBox = $false

$screen = [System.Windows.Forms.Screen]::PrimaryScreen.WorkingArea
$form.Location = New-Object System.Drawing.Point(
    ($screen.Right - $form.Width - 24),
    ($screen.Bottom - $form.Height - 48)
)

$title = New-Object System.Windows.Forms.Label
$title.Text = "Refresh World Market"
$title.Location = New-Object System.Drawing.Point(14, 14)
$title.Size = New-Object System.Drawing.Size(345, 22)
$title.Font = New-Object System.Drawing.Font("Segoe UI", 11, [System.Drawing.FontStyle]::Bold)
$form.Controls.Add($title)

$players = New-Object System.Windows.Forms.ComboBox
$players.Location = New-Object System.Drawing.Point(14, 48)
$players.Size = New-Object System.Drawing.Size(245, 28)
$players.DropDownStyle = "DropDownList"
$players.DisplayMember = "alias"
$form.Controls.Add($players)

$loadPlayers = New-Object System.Windows.Forms.Button
$loadPlayers.Text = "Reload"
$loadPlayers.Location = New-Object System.Drawing.Point(270, 47)
$loadPlayers.Size = New-Object System.Drawing.Size(90, 29)
$form.Controls.Add($loadPlayers)

$refresh = New-Object System.Windows.Forms.Button
$refresh.Text = "Refresh Shop"
$refresh.Location = New-Object System.Drawing.Point(14, 87)
$refresh.Size = New-Object System.Drawing.Size(346, 34)
$refresh.Font = New-Object System.Drawing.Font("Segoe UI", 10, [System.Drawing.FontStyle]::Bold)
$form.Controls.Add($refresh)

$status = New-Object System.Windows.Forms.Label
$status.Text = "Connecting to server..."
$status.Location = New-Object System.Drawing.Point(14, 130)
$status.Size = New-Object System.Drawing.Size(346, 20)
$status.AutoEllipsis = $true
$form.Controls.Add($status)

$keepOnTop = New-Object System.Windows.Forms.CheckBox
$keepOnTop.Text = "Always on top"
$keepOnTop.Checked = $true
$keepOnTop.Location = New-Object System.Drawing.Point(14, 151)
$keepOnTop.Size = New-Object System.Drawing.Size(180, 20)
$form.Controls.Add($keepOnTop)

$keepOnTop.Add_CheckedChanged({
    $form.TopMost = $keepOnTop.Checked
})

$loadPlayers.Add_Click({
    $loadPlayers.Enabled = $false
    try {
        $status.Text = "Loading players..."
        $players.Items.Clear()
        $playerList = @(Invoke-RestMethod -Uri "$baseUrl/admin/api/players" -Method Get -TimeoutSec 5)
        foreach ($player in $playerList) {
            [void]$players.Items.Add($player)
        }
        if ($players.Items.Count -gt 0) {
            $players.SelectedIndex = 0
            $status.Text = "$($players.Items.Count) player(s) loaded."
        } else {
            $status.Text = "No players found."
        }
    } catch {
        $status.Text = "Server unavailable: $($_.Exception.Message)"
    } finally {
        $loadPlayers.Enabled = $true
    }
})

$refresh.Add_Click({
    $selectedPlayer = $players.SelectedItem
    if ($null -eq $selectedPlayer) {
        $status.Text = "Select a player first."
        return
    }

    $refresh.Enabled = $false
    try {
        $uuid = [uri]::EscapeDataString([string]$selectedPlayer.uuid)
        $result = Invoke-RestMethod -Uri "$baseUrl/admin/api/players/$uuid/world-market/refresh" -Method Post -TimeoutSec 10
        if ($result.activeSessions -gt 0) {
            $status.Text = "Shop refreshed and sent to the game."
        } else {
            $status.Text = "Shop refreshed; player is not connected."
        }
    } catch {
        $status.Text = "Refresh failed: $($_.Exception.Message)"
    } finally {
        $refresh.Enabled = $true
    }
})

$loadPlayers.PerformClick()
[void]$form.ShowDialog()
