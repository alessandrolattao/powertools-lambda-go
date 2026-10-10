# Generates python_float_repr.json: float64 bit patterns and what Python's repr
# prints for them. Run: python3 -I python_float_repr.py > python_float_repr.json
import json, math, random, struct, sys
random.seed(20261010)
vals = []
for _ in range(300):
    bits = random.getrandbits(64)
    f = struct.unpack('<d', struct.pack('<Q', bits))[0]
    if math.isnan(f) or math.isinf(f):
        continue
    vals.append(f)
for _ in range(200):
    vals.append(round(random.uniform(-1e6, 1e6), random.randint(0, 8)))
for e in range(-8, 20):
    for m in (1, 1.5, 9.999999999999999, 2.5, 7):
        vals.append(m * 10.0**e)
        vals.append(-m * 10.0**e)
vals += [0.0, -0.0, 0.1+0.2, 1/3, 2/3, 5e-324, 2.2250738585072014e-308, 1.7976931348623157e308, 2.0**53, 2.0**53+2, 2.0**63, 123456789.125]
out = [[struct.unpack('<Q', struct.pack('<d', f))[0], repr(f)] for f in vals]
sys.stdout.write("[\n" + ",\n".join(json.dumps(p) for p in out) + "\n]\n")
