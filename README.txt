ReKapture 2.5
=============

Instant Replay and screen recording for NVIDIA GPUs, with every setting
unlocked. Video is captured with Windows' own capture API and encoded on
your GPU's NVENC chip (the same encoder the NVIDIA App uses), so the hit to
your frame rate is small.


WHAT YOU NEED
-------------
- Windows 10 (version 2004 or newer) or Windows 11, 64-bit
- An NVIDIA GeForce GTX 10-series or newer (RTX 50-series fully supported,
  including AV1)
- A current NVIDIA driver
- Microsoft Edge WebView2 Runtime (already built into Windows 11 and most
  Windows 10 PCs; ReKapture tells you if it's missing)


INSTALL
-------
There are two ways to use ReKapture. Pick one.

Installer (recommended)
1. Run ReKapture-Setup-2.5.1.exe.
2. Windows may show "Windows protected your PC" because ReKapture isn't
   code-signed. Click "More info", then "Run anyway".
3. Follow the steps. ReKapture installs to C:\Program Files (x86)\ReKapture
   unless you click Browse and pick another folder.
4. On the last page, untick "Open the README" or "Start ReKapture" if you
   don't want them, then click Finish.
Settings are kept in your AppData folder, per Windows user.
To upgrade, run a newer Setup; your settings and clips stay.
To uninstall, use Settings > Apps > Installed apps > ReKapture. It asks
whether to delete your settings too. Saved clips are never deleted.

Portable (no install)
1. Copy the "ReKapture Portable" folder anywhere you can write to: your
   Documents, another drive or a USB stick. Keep the "ffmpeg" folder next to
   ReKapture.exe.
2. Double-click ReKapture.exe (the "Run anyway" note above applies here too).
The portable version keeps its settings and log in a "Data" folder inside
its own folder, so you can move the whole folder to another PC. Delete the
file "portable.txt" to make it use AppData like the installed version.
Don't put the portable folder inside Program Files: Windows doesn't let
apps write there, so it falls back to AppData.

Use one or the other, not both at once: only one copy of ReKapture can run
at a time.


UPDATES
-------
ReKapture checks github.com/TheeBearDad/ReKapture for new versions when it
starts and every few hours. When one is out, you get a notice, and About
shows what's new with an Install update button. Nothing installs until you
click it. The download is checked against its published SHA-256 checksum.
Then ReKapture closes, updates and opens again. Windows asks for permission
once for the installed version; the portable version needs no permission.
Turn off "Check for updates automatically" on the About page to stop the
checks. You can always download a version yourself from the Releases page.


UPGRADING FROM AN EARLIER NAME
------------------------------
ReKapture is the new name for Kapture (earlier InstaReplay and GreenLight).
Quit the old app from the tray first, then install ReKapture. Your settings
carry over, and if your clips were in the old default folder
(Videos\Kapture, Videos\InstaReplay or Videos\GreenLight), it's renamed to
Videos\ReKapture. Delete the old .exe afterwards.


USING IT
--------
Instant Replay starts automatically. ReKapture keeps the last 2 minutes
(changeable, up to 20) and saves them when you press the shortcut.
When you start a game, ReKapture notices within a couple of seconds and
records the game's window, so notifications and your other screens stay out.

  Save replay ..................... Alt + Shift + F10
  Start / stop recording .......... Alt + Shift + F9
  Turn Instant Replay on or off ... Alt + Shift + X

The defaults use Shift so they don't clash with the NVIDIA App's own Alt+F10
and Alt+F9. Change any of them under "Shortcuts & overlay".

Clips are saved to Videos\ReKapture, in a folder named after the game.

Closing the window keeps ReKapture running in the system tray (the green
icon by the clock). Right-click the icon for quick actions or to quit.


SETTINGS WORTH KNOWING
----------------------
Video
- What to capture:
  Automatic (the default) follows whatever game you're playing. It records
  the game's window, or the whole monitor it's on if you prefer, and your
  chosen monitor when no game is open.
  Monitor records one monitor all the time.
  Game or app window records one program you pick.
- Games: ReKapture spots games installed by Steam, Epic, Xbox, GOG, EA,
  Ubisoft and Riot, plus anything running full screen (browsers and video
  players excluded). Every game it finds goes in the list on the Video page,
  where you can mark a program as "Not a game", or add one it missed.
  It only looks at window titles and program locations, the same things Task
  Manager shows, so it's safe with anti-cheat.
- Resolution: native, 2160p to 720p, or any custom size.
- Aspect ratio: 16:9, 21:9, 32:9, 4:3, 9:16 (vertical, for Shorts/TikTok),
  1:1, with fit, crop or stretch. Scaling happens on the GPU.
- Frame rate: 30 to 240 fps.
- Encoder: H.264, HEVC or AV1. Constant, variable or constant-quality
  bitrate, 1 to 300 Mbps, and NVENC presets from fastest to best quality.
- MP4 or MKV files.

Audio
- Game audio (any output device) and microphone (any input device).
- Discord: its own track (the default), mixed into game audio, or left out.
  You still hear Discord normally either way. This uses Windows' per-app
  audio capture (Windows 10 version 2004 or newer). While Discord is closed
  its track is silent. If Windows can't separate it, ReKapture says so and
  keeps Discord in the game audio.
- Tracks: one mixed track, separate tracks, or both (track 1 has everything
  mixed, then game, Discord and mic each on their own for editing). In the
  editor you can turn Discord down or mute it, and track 1 is rebuilt.

Recent clips (Home)
- Your newest clips with thumbnails. Click one to watch it right in the app,
  or use "Open in player" / "Show in folder".
- If a clip can't play inside the app (for example HEVC without Microsoft's
  HEVC Video Extensions, or MKV files), ReKapture makes a small preview copy
  on your GPU the first time. Preview copies and thumbnails are kept in
  %LOCALAPPDATA%\ReKapture\previews and cleared after two weeks.

Editing a clip
- Pick a clip on Home and click Edit.
- Trim: drag the green handles on the timeline, or press I and O to set the
  start and end at the playhead. "Play selection" previews just that part.
  Space plays/pauses, arrow keys skip a second, comma and period step a frame.
- Audio: change the volume (0-200%) or mute each track. For clips with
  separate game and mic tracks, the mixed track is rebuilt to match.
- Save as a new clip ("... (edited)") or replace the original (asks twice).
- Exact cut re-encodes on your GPU and cuts on the exact frame. Fast cut
  copies the video untouched, so the start snaps to the nearest 2 seconds.

Sharing to Discord
- On Home, pick a clip and click "Share to Discord". There are two ways:
- Copy to paste (no setup): choose the size to fit (20 MB free, 50 MB Nitro
  Basic, 500 MB Nitro) and click Copy. Then press Ctrl+V in any Discord chat,
  DM or server to attach the clip. Prepared copies are kept for a day in
  %LOCALAPPDATA%\ReKapture\clipboard and then deleted.
- Post to a channel: go to Sharing and add a channel: in Discord, open Edit Channel >
  Integrations > Webhooks > New Webhook > Copy Webhook URL, then paste it.
  (You need the Manage Webhooks permission. Keep the link private: anyone
  with it can post in that channel.)
  Then in Share to Discord, pick "Post to a channel", choose the channel,
  add a message, and click Post.
- Clips bigger than the server's upload limit (20 MB by default; 50/100 MB on
  boosted servers) are shrunk first on your GPU. HEVC, AV1 and MKV clips are
  converted to MP4 so they play right in the chat. Your original isn't changed.
- Very long clips can't fit at decent quality; ReKapture tells you and
  offers to open the editor so you can trim first.
- YouTube: use "Show in folder" and drag the clip into youtube.com/upload.
  (YouTube locks videos uploaded by unofficial apps to private, so there's no
  direct upload.)

Replay & saving
- When to run: Always, or Only while playing a game. With the second,
  Instant Replay waits on the desktop and turns on when a game starts.
  Alt-tabbing doesn't stop it; closing the game pauses it.

Appearance
- Change the accent color: presets, a hue bar, or any custom color. Buttons,
  highlights, the window border and in-game notices all follow it.

Overlay
- Small notices when a clip is saved and a REC timer while recording, in
  the corner of your choice. They are left out of your recordings.
- The overlay shows over borderless/windowed games. Games in exclusive
  full-screen mode can hide it; recording still works.


GOOD TO KNOW
------------
- While Instant Replay is on, a hidden ".rekapture-buffer" folder inside
  your clips folder holds the last few minutes. It's cleaned up
  automatically. Put the clips folder on a drive with free space.
- Replays and recordings are cut on 2-second boundaries, so a clip can be
  up to 2 seconds longer than asked.
- If ReKapture or your PC crashes mid-recording, the recording is saved
  the next time you open ReKapture, marked "(interrupted)".
- Both PCs in a household can run it; settings are per Windows user.


TROUBLESHOOTING
---------------
Open ReKapture, go to About, and click "Open log file". The log says what
went wrong in plain words, plus FFmpeg's own messages.

- "NVENC isn't available": update the NVIDIA driver and restart the PC.
- A shortcut doesn't work: another app owns those keys. ReKapture says so
  under "Shortcuts & overlay"; pick different keys.
- No game audio: check the output device under Audio matches where you
  actually hear sound, or leave it on "Default output device".


CREDITS
-------
Video encoding by FFmpeg (https://ffmpeg.org), licensed under the LGPL
v2.1+. The included build is from https://github.com/BtbN/FFmpeg-Builds;
see ffmpeg\LICENSE.txt.

ReKapture is an independent project. It is not affiliated with or endorsed
by NVIDIA. NVIDIA, GeForce and NVENC are trademarks of NVIDIA Corporation.
