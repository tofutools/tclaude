import test from 'node:test';
import assert from 'node:assert/strict';
import { cidrNetwork } from '../dashboard/js/cidr.js';

test('cidrNetwork masks host bits the way the daemon stores a CIDR', () => {
  assert.deepEqual(cidrNetwork('192.168.1.42/24'), { network: '192.168.1.0/24', bits: 24, single: 32, hostBits: true });
  assert.deepEqual(cidrNetwork('192.168.1.0/24'), { network: '192.168.1.0/24', bits: 24, single: 32, hostBits: false });
  assert.equal(cidrNetwork('10.1.2.3/32').hostBits, false);
  assert.equal(cidrNetwork('10.1.2.3/0').network, '0.0.0.0/0');
  assert.equal(cidrNetwork('172.16.5.4/12').network, '172.16.0.0/12');
  assert.equal(cidrNetwork('172.31.255.255/13').network, '172.24.0.0/13');
});

test('cidrNetwork reads IPv6, compressed as netip prints it', () => {
  assert.deepEqual(cidrNetwork('2001:db8::1/64'), { network: '2001:db8::/64', bits: 64, single: 128, hostBits: true });
  assert.equal(cidrNetwork('2001:db8::/32').hostBits, false);
  assert.equal(cidrNetwork('fd00:1:2:3:4:5:6:7/48').network, 'fd00:1:2::/48');
  assert.equal(cidrNetwork('::1/128').network, '::1/128');
  assert.equal(cidrNetwork('1:0:0:2:0:0:0:3/128').network, '1:0:0:2::3/128', 'the longest zero run is compressed');
  assert.equal(cidrNetwork('::ffff:10.0.0.0/120').hostBits, false, 'an embedded IPv4 tail is read');
  assert.equal(cidrNetwork('::ffff:10.0.0.9/120').hostBits, true);
  assert.equal(cidrNetwork('::ffff:10.0.0.9/120').network, '10.0.0.0/24', 'a mapped prefix names the IPv4 network the daemon stores');
  assert.equal(cidrNetwork('::ffff:0:0/96').network, '0.0.0.0/0');
});

test('cidrNetwork refuses what netip.ParsePrefix refuses', () => {
  for (const bad of ['', '10.0.0.0', '10.0.0.0/33', '10.0.0/8', '010.0.0.0/8', '256.0.0.0/8',
    '10.0.0.0/08', ' 10.0.0.0/8', '::/129', '1::2::3/64', 'fe80::1%eth0/64', 'gggg::/16',
    '1:2:3:4:5:6:7:8:9/64', '1:2:3:4:5:6:7/64']) {
    assert.equal(cidrNetwork(bad), null, bad);
  }
});
