//! Host-side forwarder for Nautilus enclaves.
//!
//! Replaces `socat TCP-LISTEN:port,fork VSOCK-CONNECT:cid:port` with an
//! async Rust binary that doesn't fork per connection.
//!
//! socat's fork-per-connection model creates a new VSOCK connection for every
//! incoming TCP connection. Under rapid sequential connections the new VSOCK
//! socket can race, causing "Transport endpoint is not connected" errors.
//! This binary uses tokio tasks instead of processes, eliminating the race.
//!
//! Usage:
//!   host-forwarder <listen-port> <enclave-cid> <vsock-port>

use tokio::io;
use tokio::net::{TcpListener, TcpStream};
use tokio_vsock::{VsockAddr, VsockStream};

#[tokio::main]
async fn main() {
    let args: Vec<String> = std::env::args().collect();
    if args.len() != 4 {
        eprintln!("Usage: {} <listen-port> <enclave-cid> <vsock-port>", args[0]);
        std::process::exit(1);
    }

    let listen_port: u16 = args[1].parse().unwrap_or_else(|_| {
        eprintln!("invalid listen port: {}", args[1]);
        std::process::exit(1);
    });
    let cid: u32 = args[2].parse().unwrap_or_else(|_| {
        eprintln!("invalid enclave CID: {}", args[2]);
        std::process::exit(1);
    });
    let vsock_port: u32 = args[3].parse().unwrap_or_else(|_| {
        eprintln!("invalid VSOCK port: {}", args[3]);
        std::process::exit(1);
    });

    let addr = format!("0.0.0.0:{listen_port}");
    let listener = TcpListener::bind(&addr).await.unwrap_or_else(|e| {
        eprintln!("failed to bind {addr}: {e}");
        std::process::exit(1);
    });

    eprintln!("[host-forwarder] TCP:{listen_port} → VSOCK:{cid}:{vsock_port}");

    loop {
        match listener.accept().await {
            Ok((tcp_stream, peer)) => {
                eprintln!("[host-forwarder] connection from {peer}");
                tokio::spawn(async move {
                    if let Err(e) = bridge(tcp_stream, cid, vsock_port).await {
                        eprintln!("[host-forwarder] bridge error: {e}");
                    }
                });
            }
            Err(e) => {
                eprintln!("[host-forwarder] accept error: {e}");
            }
        }
    }
}

async fn bridge(tcp_stream: TcpStream, cid: u32, vsock_port: u32) -> io::Result<()> {
    let vsock_stream = VsockStream::connect(VsockAddr::new(cid, vsock_port)).await?;
    let (mut tcp_r, mut tcp_w) = io::split(tcp_stream);
    let (mut vsock_r, mut vsock_w) = io::split(vsock_stream);

    let c2s = io::copy(&mut tcp_r, &mut vsock_w);
    let s2c = io::copy(&mut vsock_r, &mut tcp_w);

    tokio::select! {
        r = c2s => { r?; }
        r = s2c => { r?; }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    #[test]
    fn arg_parsing_validates_port_range() {
        assert!("8080".parse::<u16>().is_ok());
        assert!("0".parse::<u16>().is_ok());
        assert!("65535".parse::<u16>().is_ok());
        assert!("65536".parse::<u16>().is_err());
        assert!("-1".parse::<u16>().is_err());
        assert!("abc".parse::<u16>().is_err());
    }

    #[test]
    fn arg_parsing_validates_cid() {
        assert!("3".parse::<u32>().is_ok());
        assert!("16".parse::<u32>().is_ok());
        assert!("-1".parse::<u32>().is_err());
        assert!("abc".parse::<u32>().is_err());
    }
}
