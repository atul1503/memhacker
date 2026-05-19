"""
Car Speed Rotator — modifies car velocity via pointer chain at 20Hz.

Modes:
  python car_speed_rotator.py          → ROTATE (turns the car by rotating its velocity vector)
  python car_speed_rotator.py drift    → DRIFT  (adds sideways velocity, light rotation mix)

Keyboard (both modes):
  LEFT / RIGHT  steer left / right
  UP   / DOWN   boost / brake speed
  no key        steer auto-centers

Run as Administrator.

============================================================
SETUP FOR A NEW GAME — what to change in the CONFIG block
============================================================

1) PROCESS_NAME
   Set to the game's exe name (e.g. "needforspeed.exe").

2) Z_SPEED_CHAIN — find a stable pointer chain to the Z velocity float:
     - In memhacker: open <game.exe>
     - Scan for Z velocity (drive forward, scan changed/increased until 1-2 addrs)
     - pmsave s1.pmap <addr>, restart game, repeat for s2/s3
     - pscan, prsave chains.json
     - Pick a chain that survives restarts and paste as:
         ("game.exe", baseOffset, [off1, off2, ...])

3) X_SPEED_OFFSET / Y_SPEED_OFFSET / STEER_OFFSET — offsets RELATIVE to Z addr:
     - Velocity is almost always a float[3] {X, Y, Z}, so X = Z-8, Y = Z-4.
     - STEER varies per game — scan a float that changes when you turn the
       wheel, then compute (steer_addr - z_addr).

4) STEER_SENSITIVITY (ROTATE mode)
     - Start at -0.0349 (CE default for a 60Hz tick). For 20Hz like ours, try
       -0.03 to -0.05. Flip the sign if the car turns the wrong way.

5) Launch the game and run the script. Tap LEFT / RIGHT gently — if the car
   over-rotates, lower |STEER_SENSITIVITY|; if it barely turns, raise it.
"""

import ctypes
import ctypes.wintypes
import math
import time
import struct
import sys

# =============================================================================
# CONFIG
# =============================================================================

# Target process exe name (must match what shows in Task Manager).
PROCESS_NAME = "forzahorizon6.exe"

# Pointer chain to the Z velocity float: (module, baseOffset, [offsets...]).
# Resolved each tick; if it breaks the script idles until it comes back.
Z_SPEED_CHAIN = ("forzahorizon6.exe", 0xA945820, [
    0xD8,
    0xB0,
    0x0,
    0x28
])

# -----------------------------------------------------------------------------
# X / Y / Z / STEER offsets — bytes relative to the resolved Z address.
# Velocity is almost always a float[3] {X, Y, Z}, so X is 8 bytes before Z and
# Y is 4 bytes before Z. STEER lives near the velocity but the exact offset
# varies per game — scan a float that changes when you turn the wheel and
# compute (steer_addr - z_addr).
# -----------------------------------------------------------------------------
X_SPEED_OFFSET = -8
Y_SPEED_OFFSET = -4
Z_SPEED_OFFSET =  0
STEER_OFFSET   = 12

# =============================================================================
# ROTATE mode
# =============================================================================

# STEER_SENSITIVITY
#   Radians the velocity vector is rotated per (steer × tick). At 20Hz a value
#   of 0.03 means: with steer=1.0, the car rotates 0.03 rad/tick × 20 ticks/s
#   = 0.6 rad/s ≈ 34°/s. Larger magnitude = faster turn.
#   Sign flips direction — make it negative if LEFT turns the car right.
STEER_SENSITIVITY = -0.03173

# DEAD_ZONE
#   Ignore steer values with |steer| below this. Useful if you have a controller
#   or analog input adding tiny noise. With keyboard input, 0.0 is fine.
DEAD_ZONE         = 0.0

# =============================================================================
# DRIFT mode
# =============================================================================

# DRIFT_LATERAL_STRENGTH
#   How much sideways velocity to add per (steer unit × tick). Bigger = more
#   pronounced sideways slide. 5.0 is a moderate drift; try 2.0 for subtle.
DRIFT_LATERAL_STRENGTH = 5.0

# DRIFT_ROTATE_MIX
#   How much the velocity vector is *also* rotated during a drift (radians per
#   steer unit). 0 = pure sideways slide, no turning. Small values like 0.03
#   let the car gradually rotate into the slide so it doesn't feel sticky.
DRIFT_ROTATE_MIX       = 0.03

# DRIFT_SPEED_MIN
#   Don't drift below this speed (units = game units/s, same as raw velocity).
#   Drift physics misbehave at very low speed; this skips the effect when
#   nearly stopped.
DRIFT_SPEED_MIN        = 2.0

# =============================================================================
# Keyboard steering
# =============================================================================

# STEER_KEY_STEP
#   How much `steer` changes per tick while LEFT/RIGHT is held — a constant
#   linear step. Bigger = faster wheel-turn input. 0.1 means 10 ticks (0.5s
#   at 20Hz) to go from steer=0 to steer=1.
STEER_KEY_STEP    = 0.1

# STEER_DAMP
#   Auto-center exponential divisor — applied every tick when NO arrow is held.
#   `steer = steer / STEER_DAMP` each tick, so 1.1 means steer shrinks 9%/tick
#   (decays curvy: fast at first, slow near 0). 1.0 = disabled, no centering.
#   Bigger = snaps back to 0 faster.
STEER_DAMP        = 1.1

# STEER_MAX
#   Hard cap on |steer|. Without this, holding LEFT for 10 seconds would push
#   steer to a huge number, and on release it would take forever to decay back.
#   2.0 means the car can over-saturate beyond the natural [-1,1] range but
#   not by much.
STEER_MAX         = 2.0

# STEER_WRITE_MODE
#   How the script's steer value interacts with the game's own steer input:
#     "add" — read the game's current steer and write (game + script). Your
#             wheel/controller/in-game keys still work; the script just nudges
#             on top. When the script is idle (|steer|<0.001) we don't write
#             at all, leaving the game's value completely untouched.
#     "set" — overwrite the game's value with the script's `steer` every tick.
#             Script fully owns steering; game input is ignored.
#   Note: regardless of mode, velocity rotation in run_rotate / run_drift
#   uses only the script's `steer` — so the rotator effect always adds on top
#   of the game's natural physics.
STEER_WRITE_MODE  = "add"

# -----------------------------------------------------------------------------
# Steer reversal (hybrid exponential → linear when changing direction)
# -----------------------------------------------------------------------------
# When you press LEFT while currently steering right (steer > 0), or RIGHT
# while currently steering left (steer < 0), we apply exponential decay first
# (fast at high |steer|) and only switch to linear stepping once |steer| drops
# below the threshold. Result: direction changes feel much snappier than the
# plain linear step alone.

# STEER_REVERSAL_ENABLED
#   Master switch. False = always use the linear step (old behavior, sluggish
#   reversal). True = use the hybrid scheme described above.
STEER_REVERSAL_ENABLED   = True

# STEER_REVERSAL_DAMP
#   Divisor used during the exponential phase of a reversal. Bigger value =
#   faster snap toward 0 when you change direction. Kept separate from
#   STEER_DAMP (auto-center) so you can tune reversal more aggressively
#   without making the no-key auto-center feel jumpy.
STEER_REVERSAL_DAMP      = 1.1

# STEER_REVERSAL_THRESHOLD
#   |steer| at which we switch from exponential divide to linear step during
#   reversal. Below this, exponential drops would be smaller than a linear
#   step, so linear is faster.
#   Math: per-tick exp drop = steer × (1 - 1/STEER_REVERSAL_DAMP). That equals
#   STEER_KEY_STEP when steer = STEER_KEY_STEP / (1 - 1/STEER_REVERSAL_DAMP).
#   With defaults (step=0.1, damp=1.1) that's 1.1 — the math crossover.
#   Lower threshold = transition into linear earlier (smoother, less abrupt).
#   Higher threshold = stay in exponential longer (snappier, more sudden).
STEER_REVERSAL_THRESHOLD = 1.1

# =============================================================================
# Speed keys
# =============================================================================

# SPEED_BOOST_MULT
#   UP arrow: multiplies velocity magnitude by this per tick. 1.01 at 20Hz is
#   ~+22% per second. Bigger = more aggressive boost.
SPEED_BOOST_MULT  = 1.01

# SPEED_BRAKE_MULT
#   DOWN arrow: same idea but <1.0 to shrink velocity. 0.97 at 20Hz is ~−46%
#   per second. Smaller = harder brake.
SPEED_BRAKE_MULT  = 0.97

# Tick rate. 20Hz = 50ms per tick.
UPDATE_HZ = 20

# =============================================================================
# Windows API
# =============================================================================

k32 = ctypes.windll.kernel32
user32 = ctypes.windll.user32

PROCESS_ALL_ACCESS  = 0x1F0FFF
TH32CS_SNAPPROCESS  = 0x2
TH32CS_SNAPMODULE   = 0x8
TH32CS_SNAPMODULE32 = 0x10

VK_LEFT  = 0x25
VK_RIGHT = 0x27
VK_UP    = 0x26
VK_DOWN  = 0x28

class PROCESSENTRY32(ctypes.Structure):
    _fields_ = [
        ("dwSize",              ctypes.wintypes.DWORD),
        ("cntUsage",            ctypes.wintypes.DWORD),
        ("th32ProcessID",       ctypes.wintypes.DWORD),
        ("th32DefaultHeapID",   ctypes.POINTER(ctypes.c_ulong)),
        ("th32ModuleID",        ctypes.wintypes.DWORD),
        ("cntThreads",          ctypes.wintypes.DWORD),
        ("th32ParentProcessID", ctypes.wintypes.DWORD),
        ("pcPriClassBase",      ctypes.c_long),
        ("dwFlags",             ctypes.wintypes.DWORD),
        ("szExeFile",           ctypes.c_char * 260),
    ]

class MODULEENTRY32(ctypes.Structure):
    _fields_ = [
        ("dwSize",        ctypes.wintypes.DWORD),
        ("th32ModuleID",  ctypes.wintypes.DWORD),
        ("th32ProcessID", ctypes.wintypes.DWORD),
        ("GlblcntUsage",  ctypes.wintypes.DWORD),
        ("ProccntUsage",  ctypes.wintypes.DWORD),
        ("modBaseAddr",   ctypes.POINTER(ctypes.c_byte)),
        ("modBaseSize",   ctypes.wintypes.DWORD),
        ("hModule",       ctypes.wintypes.HMODULE),
        ("szModule",      ctypes.c_char * 256),
        ("szExePath",     ctypes.c_char * 260),
    ]

# =============================================================================
# Memory helpers
# =============================================================================

def get_pid(name):
    snap = k32.CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0)
    entry = PROCESSENTRY32()
    entry.dwSize = ctypes.sizeof(entry)
    k32.Process32First(snap, ctypes.byref(entry))
    while True:
        if entry.szExeFile.decode(errors="ignore").lower() == name.lower():
            k32.CloseHandle(snap)
            return entry.th32ProcessID
        if not k32.Process32Next(snap, ctypes.byref(entry)):
            break
    k32.CloseHandle(snap)
    return 0

def get_module_base(pid, name):
    snap = k32.CreateToolhelp32Snapshot(TH32CS_SNAPMODULE | TH32CS_SNAPMODULE32, pid)
    entry = MODULEENTRY32()
    entry.dwSize = ctypes.sizeof(entry)
    k32.Module32First(snap, ctypes.byref(entry))
    while True:
        if entry.szModule.decode(errors="ignore").lower() == name.lower():
            base = ctypes.cast(entry.modBaseAddr, ctypes.c_void_p).value
            k32.CloseHandle(snap)
            return base
        if not k32.Module32Next(snap, ctypes.byref(entry)):
            break
    k32.CloseHandle(snap)
    return 0

def read_bytes(handle, addr, n):
    buf = ctypes.create_string_buffer(n)
    read = ctypes.c_size_t(0)
    k32.ReadProcessMemory(handle, ctypes.c_void_p(addr), buf, n, ctypes.byref(read))
    return buf.raw[:read.value]

def read_float(handle, addr):
    data = read_bytes(handle, addr, 4)
    return struct.unpack('<f', data)[0] if len(data) >= 4 else 0.0

def read_ptr(handle, addr):
    data = read_bytes(handle, addr, 8)
    return struct.unpack('<Q', data)[0] if len(data) >= 8 else 0

def write_float(handle, addr, value):
    buf = struct.pack('<f', value)
    written = ctypes.c_size_t(0)
    k32.WriteProcessMemory(handle, ctypes.c_void_p(addr),
                           ctypes.c_char_p(buf), 4, ctypes.byref(written))

def resolve_chain(handle, module_base, base_offset, offsets):
    addr = module_base + base_offset
    for off in offsets:
        ptr = read_ptr(handle, addr)
        if ptr == 0:
            return 0
        addr = ptr + off
    return addr

def key_down(vk):
    return bool(user32.GetAsyncKeyState(vk) & 0x8000)

def normalize_speed(new_vx, new_vz, original_speed):
    new_speed = math.sqrt(new_vx*new_vx + new_vz*new_vz)
    if new_speed > 0.001:
        scale = original_speed / new_speed
        return new_vx * scale, new_vz * scale
    return new_vx, new_vz

# =============================================================================
# Physics helpers
# =============================================================================

def rotate_xz(vx, vz, angle_rad):
    c, s = math.cos(angle_rad), math.sin(angle_rad)
    return vx * c - vz * s, vx * s + vz * c

def lateral_vector(vx, vz):
    speed = math.sqrt(vx*vx + vz*vz)
    if speed < 0.001:
        return 0.0, 0.0
    return -vz / speed, vx / speed

# =============================================================================
# Keyboard steer management
# =============================================================================

def apply_speed_keys(handle, z_addr, vx, vz):
    up   = key_down(VK_UP)
    down = key_down(VK_DOWN)
    if not up and not down:
        return vx, vz
    speed = math.sqrt(vx*vx + vz*vz)
    if speed < 0.001:
        return vx, vz
    mult = SPEED_BOOST_MULT if up else SPEED_BRAKE_MULT
    return vx * mult, vz * mult

def update_keyboard_steer(current_steer):
    left  = key_down(VK_LEFT)
    right = key_down(VK_RIGHT)

    if left and not right:
        # Reversal: still steering RIGHT (positive). Damp toward 0 while we're
        # far from zero (exp wins), then linear step into LEFT (linear wins near 0).
        if STEER_REVERSAL_ENABLED and current_steer > STEER_REVERSAL_THRESHOLD:
            new_steer = current_steer / STEER_REVERSAL_DAMP
        else:
            new_steer = current_steer - STEER_KEY_STEP
    elif right and not left:
        if STEER_REVERSAL_ENABLED and current_steer < -STEER_REVERSAL_THRESHOLD:
            new_steer = current_steer / STEER_REVERSAL_DAMP
        else:
            new_steer = current_steer + STEER_KEY_STEP
    else:
        new_steer = current_steer / STEER_DAMP
        if abs(new_steer) < 0.001:
            new_steer = 0.0

    if new_steer > STEER_MAX: new_steer = STEER_MAX
    if new_steer < -STEER_MAX: new_steer = -STEER_MAX
    return new_steer

# =============================================================================
# Modes
# =============================================================================

def run_rotate(handle, z_addr, steer):
    vx    = read_float(handle, z_addr + X_SPEED_OFFSET)
    vz    = read_float(handle, z_addr + Z_SPEED_OFFSET)
    original_speed = math.sqrt(vx*vx + vz*vz)

    if abs(steer) > DEAD_ZONE and original_speed > 0.1:
        angle = steer * STEER_SENSITIVITY
        new_vx, new_vz = rotate_xz(vx, vz, angle)
        new_vx, new_vz = normalize_speed(new_vx, new_vz, original_speed)
        write_float(handle, z_addr + X_SPEED_OFFSET, new_vx)
        write_float(handle, z_addr + Z_SPEED_OFFSET, new_vz)
        direction = "LEFT " if steer < 0 else "RIGHT"
        return (f"[ROTATE] steer={steer:+.3f} [{direction}]  "
                f"vx:{vx:+.2f}->{new_vx:+.2f}  vz:{vz:+.2f}->{new_vz:+.2f}  "
                f"speed={original_speed:.2f}")
    return f"[ROTATE] steer={steer:+.3f} [--]  vx={vx:+.2f}  vz={vz:+.2f}  speed={original_speed:.2f}"


def run_drift(handle, z_addr, steer):
    vx    = read_float(handle, z_addr + X_SPEED_OFFSET)
    vz    = read_float(handle, z_addr + Z_SPEED_OFFSET)
    original_speed = math.sqrt(vx*vx + vz*vz)

    if abs(steer) > DEAD_ZONE and original_speed > DRIFT_SPEED_MIN:
        lat_x, lat_z = lateral_vector(vx, vz)
        lateral_amount = steer * DRIFT_LATERAL_STRENGTH
        new_vx = vx + lat_x * lateral_amount
        new_vz = vz + lat_z * lateral_amount
        if abs(DRIFT_ROTATE_MIX) > 0:
            new_vx, new_vz = rotate_xz(new_vx, new_vz, steer * DRIFT_ROTATE_MIX)
        new_vx, new_vz = normalize_speed(new_vx, new_vz, original_speed)
        write_float(handle, z_addr + X_SPEED_OFFSET, new_vx)
        write_float(handle, z_addr + Z_SPEED_OFFSET, new_vz)
        direction = "LEFT " if steer < 0 else "RIGHT"
        return (f"[DRIFT ] steer={steer:+.3f} [{direction}]  "
                f"lateral={lateral_amount:+.2f}  "
                f"vx:{vx:+.2f}->{new_vx:+.2f}  vz:{vz:+.2f}->{new_vz:+.2f}  "
                f"speed={original_speed:.2f}")
    return f"[DRIFT ] steer={steer:+.3f} [--]  speed={original_speed:.2f}"

# =============================================================================
# Main
# =============================================================================

def main():
    mode = "drift" if len(sys.argv) > 1 and sys.argv[1].lower() == "drift" else "rotate"

    print(f"[*] Mode: {mode.upper()}")
    print(f"[*] Keys: LEFT/RIGHT = steer | UP = boost speed | DOWN = reduce speed | release = auto-center")
    print(f"[*] Looking for {PROCESS_NAME}...")

    pid = get_pid(PROCESS_NAME)
    if not pid:
        print(f"[!] '{PROCESS_NAME}' not found. Is the game running?")
        sys.exit(1)
    print(f"[+] PID: {pid}")

    handle = k32.OpenProcess(PROCESS_ALL_ACCESS, False, pid)
    if not handle:
        print("[!] Failed to open process. Run as Administrator.")
        sys.exit(1)

    module_name, base_offset, chain_offsets = Z_SPEED_CHAIN
    module_base = get_module_base(pid, module_name)
    if not module_base:
        print(f"[!] Module '{module_name}' not found.")
        k32.CloseHandle(handle)
        sys.exit(1)
    print(f"[+] Module base: 0x{module_base:X}")
    print(f"[*] Running at {UPDATE_HZ}Hz. Ctrl+C to stop.\n")

    interval = 1.0 / UPDATE_HZ
    steer = 0.0  # our keyboard-controlled steer value

    try:
        while True:
            t0 = time.perf_counter()

            z_addr = resolve_chain(handle, module_base, base_offset, chain_offsets)
            if z_addr == 0:
                print("[!] Pointer chain broken — waiting...", end="\r")
                time.sleep(1.0)
                continue

            steer = update_keyboard_steer(steer)

            if STEER_WRITE_MODE == "add":
                if abs(steer) > 0.001:
                    game_steer = read_float(handle, z_addr + STEER_OFFSET)
                    write_float(handle, z_addr + STEER_OFFSET, game_steer + steer)
            else:
                write_float(handle, z_addr + STEER_OFFSET, steer)

            if mode == "drift":
                status = run_drift(handle, z_addr, steer)
            else:
                status = run_rotate(handle, z_addr, steer)

            vx = read_float(handle, z_addr + X_SPEED_OFFSET)
            vz = read_float(handle, z_addr + Z_SPEED_OFFSET)
            new_vx, new_vz = apply_speed_keys(handle, z_addr, vx, vz)
            if new_vx != vx or new_vz != vz:
                write_float(handle, z_addr + X_SPEED_OFFSET, new_vx)
                write_float(handle, z_addr + Z_SPEED_OFFSET, new_vz)

            print(f"\r  {status}    ", end="", flush=True)

            elapsed = time.perf_counter() - t0
            sleep_t = interval - elapsed
            if sleep_t > 0:
                time.sleep(sleep_t)

    except KeyboardInterrupt:
        print("\n[*] Stopped.")
    finally:
        k32.CloseHandle(handle)

if __name__ == "__main__":
    main()
