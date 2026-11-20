# Paradise Bay server

In 2015, King/Z2 made the "Paradise Bay" game which they discontinued in 2018, with servers closing. At the time I was playing this game. I wanted to re-play it but couldn't as the servers closed.

Fortunately, the servers URLs are just in an array in the `game-info.json` file, which is stored in the Appx package (on Windows).

![](./screenshot.png)
The game running on Windows 11 25H2 and being debugged on Visual Studio 2022.

## Known issues

- Some things may not work, not all features have been re-implemented
- Remote assets (assets that were on King servers) cannot be re-added since the original servers don't exist anymore

## Usage with the game

### Downloading the game `.appx`

A `.appx` file is a Microsoft Store package file. Downloading it will allow us to edit its contents.

1. Go to https://store.rg-adguard.net/
2. Filter by ProductId, search for `9nblggh5l706`
3. Download `king.com.ParadiseBay_3.9.0.0_x86__kgqvnymyfvs32.appx` (the last file) and move it in this folder.

### Installing the game

1. Enable "Developer Mode": Go to Settings > System > For developers > Enable "Developer Mode"
2. Run `install.bat`

If you have any issues while installing or playing the game don't hesitate to [open a GitHub issue](https://github.com/CuteTenshii/paradise-bay-server/issues/new)!!

### Starting the server

You need to install [Go](https://go.dev/dl/) to run it. Then, run `start.bat`.

### Managing player saves

With the server running, open [http://127.0.0.1:3300/admin](http://127.0.0.1:3300/admin) on the same computer. Select a player to view saved fragments, change their alias or link a new device ID (`z2did`), and download or restore a JSON backup. Restoring replaces the player's profile and saved fragments.

After reinstalling, if the game creates a new `Guest` account, stop the game first. Copy the new account's `z2did` from the player list, delete that temporary account in the panel, then select the old account and save the copied `z2did`. Download a backup before deleting an account: deletion removes its profile, fragments, and transaction history.

The server automatically skips the final seal-house dive tutorial missions for existing and new players. If the game is already open during the first server restart after updating, restart the game to load the updated mission progress. The selected player's panel also has an **Unlock camera (skip tutorial)** action; download a backup and stop the game before using it manually.

The World Market is simulated locally with 14 randomized offers per player, refreshed every five minutes by default. In the admin panel, the **World Market** controls let you set a server-wide refresh interval (1–1440 minutes) and offer count (1–27), then immediately regenerate the selected player's shop. Settings persist across server restarts; changes apply to each player's next refresh. Purchases from these offers are completed locally and the item is delivered through the game's mail system. The server does not connect players to a shared online market, and visiting simulated sellers is not implemented.

To refresh the selected player's shop without leaving the game, start `world-market-overlay.bat` after the server is running. The small always-on-top window lets you select a player and send the refresh directly to their connected game session. Use **Reload** if a player was added while the window was open.

In the admin panel, **Island Appearance** has a ground snow toggle for the main island. The server saves the original island composition beside the game data before removing the snow component, so it can be restored later. Close and relaunch the game after changing the setting; this does not alter player saves or the server database.

The admin panel and its API are available only from the same computer. The game HTTP endpoints continue to use their existing network binding.