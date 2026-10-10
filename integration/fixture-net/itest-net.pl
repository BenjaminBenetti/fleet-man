#!/usr/bin/env perl
# itest-net.pl - the in-instance end of the network itests (port forwarding,
# the browser proxy, the Fleet Launch landing page). Core perl only
# (IO::Socket::INET, Digest::SHA), which the debian devcontainer base image
# ships; tests start it detached with `fleet exec` (see integration/net/net.sh).
#
# Usage: perl itest-net.pl <mode> <port> [bind-address] [file]
#
#   http    HTTP/1.1, one request per connection:
#             /         "fleet-itest-web <port>"
#             /health   "healthy"
#             /cookies  three Set-Cookie headers in one response
#             /ws       WebSocket upgrade, then echoes one text frame as
#                       "echo:<text>" and closes
#   sink    reads to EOF, then replies "bytes=<n> sha256=<hex>" and closes:
#           the reply only exists if the client's half-close got through
#   source  sends <file>, then closes
#   echo    echoes every byte until EOF, then closes
#
# bind-address defaults to 0.0.0.0. Each connection is served in a child.
use strict;
use warnings;
use IO::Socket::INET;
use Digest::SHA qw(sha1_base64);

my ($mode, $port, $bind, $file) = @ARGV;
die "usage: itest-net.pl <http|sink|source|echo> <port> [bind] [file]\n"
  unless $mode && $port && $mode =~ /^(http|sink|source|echo)$/;
$bind ||= '0.0.0.0';
die "source needs a file\n" if $mode eq 'source' && !($file && -r $file);

my $server = IO::Socket::INET->new(
  LocalAddr => $bind, LocalPort => $port, Proto => 'tcp',
  Listen => 64, ReuseAddr => 1,
) or die "listen $bind:$port: $!\n";
print STDERR "itest-net: $mode listening on $bind:$port\n";
$SIG{CHLD} = 'IGNORE';

while (1) {
  my $conn = $server->accept or next;
  my $pid = fork;
  if (!defined $pid) { close $conn; next; }
  if ($pid == 0) {
    close $server;
    binmode $conn;
    $conn->autoflush(1);
    if    ($mode eq 'http')   { serve_http($conn) }
    elsif ($mode eq 'sink')   { serve_sink($conn) }
    elsif ($mode eq 'source') { serve_source($conn) }
    else                      { serve_echo($conn) }
    close $conn;
    exit 0;
  }
  close $conn;
}

sub serve_sink {
  my ($conn) = @_;
  my $sha = Digest::SHA->new(256);
  my ($n, $buf) = (0, '');
  while (my $r = sysread($conn, $buf, 65536)) { $n += $r; $sha->add($buf); }
  syswrite_all($conn, "bytes=$n sha256=" . $sha->hexdigest . "\n");
}

sub serve_source {
  my ($conn) = @_;
  open(my $fh, '<', $file) or die "open $file: $!\n";
  binmode $fh;
  my $buf;
  while (my $r = sysread($fh, $buf, 65536)) { syswrite_all($conn, $buf) or last; }
  close $fh;
}

sub serve_echo {
  my ($conn) = @_;
  my $buf;
  while (my $r = sysread($conn, $buf, 65536)) { syswrite_all($conn, $buf) or last; }
}

sub syswrite_all {
  my ($conn, $data) = @_;
  while (length $data) {
    my $w = syswrite($conn, $data);
    return 0 unless defined $w && $w > 0;
    substr($data, 0, $w) = '';
  }
  return 1;
}

sub serve_http {
  my ($conn) = @_;
  my $line = <$conn>;
  return unless defined $line;
  $line =~ s/\r?\n\z//;
  my (undef, $target) = split / /, $line;
  my %h;
  while (my $l = <$conn>) {
    $l =~ s/\r?\n\z//;
    last if $l eq '';
    my ($k, $v) = split /:\s*/, $l, 2;
    $h{lc $k} = $v // '';
  }
  $target = '/' unless defined $target;
  $target =~ s{^[a-z]+://[^/]*}{}i;    # absolute-form, as a proxy may send it
  my $path = $target;
  $path =~ s/\?.*//;
  $path = '/' if $path eq '';

  if ($path eq '/ws' && lc($h{upgrade} // '') eq 'websocket') {
    return serve_websocket($conn, $h{'sec-websocket-key'} // '');
  }
  my @headers;
  my $status = '200 OK';
  my $body;
  if ($path eq '/') {
    $body = "fleet-itest-web $port\n";
  } elsif ($path eq '/health') {
    $body = "healthy\n";
  } elsif ($path eq '/cookies') {
    @headers = (
      'Set-Cookie: fleet_a=1; Path=/',
      'Set-Cookie: fleet_b=2; Path=/; HttpOnly',
      'Set-Cookie: fleet_c=3; Path=/; Max-Age=600',
    );
    $body = "three cookies\n";
  } else {
    $status = '404 Not Found';
    $body = "not found: $path\n";
  }
  print $conn "HTTP/1.1 $status\r\n",
    "Content-Type: text/plain\r\n",
    "Content-Length: " . length($body) . "\r\n",
    "Connection: close\r\n",
    (map { "$_\r\n" } @headers),
    "\r\n", $body;
}

# serve_websocket: RFC 6455 handshake, one masked client text frame in, one
# "echo:<text>" frame and a close frame out.
sub serve_websocket {
  my ($conn, $key) = @_;
  my $accept = sha1_base64($key . '258EAFA5-E914-47DA-95CA-C5AB0DC85B11') . '=';
  print $conn "HTTP/1.1 101 Switching Protocols\r\n",
    "Upgrade: websocket\r\n",
    "Connection: Upgrade\r\n",
    "Sec-WebSocket-Accept: $accept\r\n\r\n";
  my $hdr;
  return unless read($conn, $hdr, 2) == 2;
  my (undef, $b1) = unpack 'CC', $hdr;
  my $len = $b1 & 0x7f;
  my $ext;
  if ($len == 126) { read($conn, $ext, 2); $len = unpack 'n', $ext; }
  elsif ($len == 127) { read($conn, $ext, 8); $len = unpack 'Q>', $ext; }
  my $mask = '';
  read($conn, $mask, 4) if $b1 & 0x80;
  my $payload = '';
  return if $len && read($conn, $payload, $len) != $len;
  if (length $mask) {
    my @m = unpack 'C4', $mask;
    my @p = unpack 'C*', $payload;
    $p[$_] ^= $m[$_ % 4] for 0 .. $#p;
    $payload = pack 'C*', @p;
  }
  my $reply = "echo:$payload";
  print $conn pack('CC', 0x81, length $reply), $reply, pack('CC', 0x88, 0);
}
