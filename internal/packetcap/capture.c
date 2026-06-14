#include "capture.h"
#include <string.h>
#include <stdlib.h>

#if defined(__linux__)
#include <unistd.h>
#include <sys/socket.h>
#include <sys/mman.h>
#include <sys/ioctl.h>
#include <net/if.h>
#include <linux/if_packet.h>
#include <linux/if_ether.h>
#include <arpa/inet.h>
#include <time.h>
#include <errno.h>

struct capture_handle {
    int fd;
    char ifname[PCAP_IFNAME_LEN];
    uint16_t ethertype_filter;
    int use_zero_copy;
    struct capture_stats stats;
    void* ring_base;
    size_t ring_size;
    int block_idx;
    int frame_idx;
    int sim_mode;
    uint32_t sim_smpcnt;
};

static int setup_tpacket_v3(struct capture_handle* h) {
    int version = TPACKET_V3;
    if (setsockopt(h->fd, SOL_PACKET, PACKET_VERSION, &version, sizeof(version)) < 0) {
        return -1;
    }
    struct tpacket_req3 req;
    memset(&req, 0, sizeof(req));
    req.tp_block_size = PCAP_RING_BLOCKSZ;
    req.tp_block_nr   = PCAP_RING_BLOCKS;
    req.tp_frame_size = TPACKET_ALIGN(PCAP_FRAME_MAX + 128);
    req.tp_frame_nr   = PCAP_RING_FRAMES * PCAP_RING_BLOCKS;
    req.tp_retire_blk_tov = 1000;
    req.tp_feature_req_word = TP_FT_REQ_FILL_RXHASH;
    if (setsockopt(h->fd, SOL_PACKET, PACKET_RX_RING, &req, sizeof(req)) < 0) {
        return -1;
    }
    size_t ring_sz = (size_t)req.tp_block_size * (size_t)req.tp_block_nr;
    h->ring_base = mmap(NULL, ring_sz, PROT_READ | PROT_WRITE, MAP_SHARED | MAP_LOCKED, h->fd, 0);
    if (h->ring_base == MAP_FAILED) {
        return -1;
    }
    h->ring_size = ring_sz;
    h->block_idx = 0;
    h->frame_idx = 0;
    return 0;
}

static int bind_to_iface(struct capture_handle* h) {
    struct ifreq ifr;
    memset(&ifr, 0, sizeof(ifr));
    strncpy(ifr.ifr_name, h->ifname, IFNAMSIZ - 1);
    if (ioctl(h->fd, SIOCGIFINDEX, &ifr) < 0) return -1;
    int ifidx = ifr.ifr_ifindex;
    struct sockaddr_ll sll;
    memset(&sll, 0, sizeof(sll));
    sll.sll_family = AF_PACKET;
    sll.sll_protocol = htons(h->ethertype_filter);
    sll.sll_ifindex = ifidx;
    if (bind(h->fd, (struct sockaddr*)&sll, sizeof(sll)) < 0) return -1;
    if (ioctl(h->fd, SIOCGIFFLAGS, &ifr) < 0) return -1;
    ifr.ifr_flags |= IFF_UP | IFF_RUNNING;
    if (h->use_zero_copy & 0x02) {
        ifr.ifr_flags |= IFF_PROMISC;
    }
    if (ioctl(h->fd, SIOCSIFFLAGS, &ifr) < 0) return -1;
    return 0;
}

struct capture_handle* capture_open(const char* ifname,
                                    uint16_t ethertype_filter,
                                    int promisc,
                                    int use_zero_copy) {
    struct capture_handle* h = (struct capture_handle*)calloc(1, sizeof(*h));
    if (!h) return NULL;
    strncpy(h->ifname, ifname, PCAP_IFNAME_LEN - 1);
    h->ethertype_filter = ethertype_filter;
    h->use_zero_copy = use_zero_copy | (promisc ? 0x02 : 0);
    h->fd = socket(AF_PACKET, SOCK_RAW, htons(ethertype_filter));
    if (h->fd < 0) {
        h->sim_mode = 1;
        h->sim_smpcnt = 0;
        return h;
    }
    if (bind_to_iface(h) < 0) {
        close(h->fd);
        h->fd = -1;
        h->sim_mode = 1;
        return h;
    }
    if (use_zero_copy) {
        if (setup_tpacket_v3(h) < 0) {
            h->use_zero_copy = 0;
        }
    }
    return h;
}

static uint64_t mono_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC_RAW, &ts);
    return (uint64_t)ts.tv_sec * 1000000000ULL + (uint64_t)ts.tv_nsec;
}

int capture_next_frame(struct capture_handle* h, struct frame_buf* out) {
    if (!h || !out) return -1;
    if (h->sim_mode) return capture_simulate(h, out, 1, 80, h->sim_smpcnt);
    if (h->use_zero_copy && h->ring_base) {
        struct tpacket_block_desc* bd =
            (struct tpacket_block_desc*)((char*)h->ring_base +
                                          h->block_idx * PCAP_RING_BLOCKSZ);
        while (!(bd->hdr.bh1.block_status & TP_STATUS_USER)) {
            ioctl(h->fd, SIOCGSTAMP, NULL);
        }
        if (h->frame_idx >= (int)bd->hdr.bh1.num_pkts) {
            bd->hdr.bh1.block_status = TP_STATUS_KERNEL;
            h->block_idx = (h->block_idx + 1) % PCAP_RING_BLOCKS;
            h->frame_idx = 0;
            return -EAGAIN;
        }
        struct tpacket3_hdr* ppd = (struct tpacket3_hdr*)((char*)bd + bd->hdr.bh1.offset_to_first_pkt);
        for (int i = 0; i < h->frame_idx; i++) {
            ppd = (struct tpacket3_hdr*)((char*)ppd + ppd->tp_next_offset);
        }
        out->rx_time_ns = mono_ns();
        out->length = ppd->tp_snaplen;
        if (out->length > PCAP_FRAME_MAX) out->length = PCAP_FRAME_MAX;
        memcpy(out->data, (char*)ppd + ppd->tp_mac, out->length);
        h->frame_idx++;
        h->stats.rx_packets++;
        h->stats.rx_bytes += out->length;
        return 1;
    }
    struct sockaddr_ll sll;
    socklen_t slen = sizeof(sll);
    ssize_t n = recvfrom(h->fd, out->data, PCAP_FRAME_MAX, 0,
                         (struct sockaddr*)&sll, &slen);
    if (n < 0) return -1;
    out->length = (uint32_t)n;
    out->rx_time_ns = mono_ns();
    h->stats.rx_packets++;
    h->stats.rx_bytes += (uint64_t)n;
    return 1;
}

int capture_read_batch(struct capture_handle* h,
                       struct frame_buf* out_batch,
                       int batch_size) {
    if (!h || !out_batch || batch_size <= 0) return -1;
    if (h->sim_mode) return capture_simulate(h, out_batch, batch_size, 80, h->sim_smpcnt);
    int count = 0;
    for (int i = 0; i < batch_size; i++) {
        int r = capture_next_frame(h, &out_batch[i]);
        if (r > 0) count++;
        else break;
    }
    return count;
}

#else

#include <time.h>
#include <stdio.h>

struct capture_handle {
    char ifname[PCAP_IFNAME_LEN];
    uint16_t ethertype_filter;
    int use_zero_copy;
    struct capture_stats stats;
    int sim_mode;
    uint32_t sim_smpcnt;
    int fd;
};

static uint64_t mono_ns(void) {
    struct timespec ts;
#ifdef _WIN32
    extern int clock_gettime_monotonic(struct timespec* ts);
    clock_gettime_monotonic(&ts);
#else
    clock_gettime(CLOCK_MONOTONIC, &ts);
#endif
    return (uint64_t)ts.tv_sec * 1000000000ULL + (uint64_t)ts.tv_nsec;
}

struct capture_handle* capture_open(const char* ifname,
                                    uint16_t ethertype_filter,
                                    int promisc,
                                    int use_zero_copy) {
    struct capture_handle* h = (struct capture_handle*)calloc(1, sizeof(*h));
    if (!h) return NULL;
    strncpy(h->ifname, ifname, PCAP_IFNAME_LEN - 1);
    h->ethertype_filter = ethertype_filter;
    h->use_zero_copy = use_zero_copy;
    h->sim_mode = 1;
    h->sim_smpcnt = 0;
    h->fd = -1;
    (void)promisc;
    return h;
}

int capture_next_frame(struct capture_handle* h, struct frame_buf* out) {
    if (!h || !out) return -1;
    return capture_simulate(h, out, 1, 80, h->sim_smpcnt);
}

int capture_read_batch(struct capture_handle* h,
                       struct frame_buf* out_batch,
                       int batch_size) {
    if (!h || !out_batch || batch_size <= 0) return -1;
    return capture_simulate(h, out_batch, batch_size, 80, h->sim_smpcnt);
}

#endif

static void build_sv_frame(uint8_t* data, uint32_t* out_len, uint32_t smpcnt, int num_samples) {
    int pos = 0;
    for (int i = 0; i < 6; i++) data[pos++] = 0x01;
    for (int i = 0; i < 6; i++) data[pos++] = 0x02;
    data[pos++] = 0x88; data[pos++] = 0xBA;
    data[pos++] = 0x00; data[pos++] = 0x01;
    uint16_t payload_len = (uint16_t)(num_samples * 8 + 128);
    data[pos++] = (uint8_t)(payload_len >> 8);
    data[pos++] = (uint8_t)(payload_len & 0xFF);
    data[pos++] = 0x00; data[pos++] = 0x00;
    data[pos++] = 0x00; data[pos++] = 0x00;
    data[pos++] = 0x60;
    uint8_t apdu_len_l1 = (uint8_t)(payload_len - 8 + 2);
    if (apdu_len_l1 < 0x80) {
        data[pos++] = apdu_len_l1;
    } else {
        data[pos++] = 0x81;
        data[pos++] = apdu_len_l1;
    }
    data[pos++] = 0x80; data[pos++] = 0x01; data[pos++] = 0x01;
    data[pos++] = 0xA2;
    uint8_t seq_asdu_len = (uint8_t)(num_samples * 8 + 96);
    if (seq_asdu_len < 0x80) {
        data[pos++] = seq_asdu_len;
    } else {
        data[pos++] = 0x81;
        data[pos++] = seq_asdu_len;
    }
    data[pos++] = 0x30;
    uint8_t asdu_len = (uint8_t)(num_samples * 8 + 90);
    if (asdu_len < 0x80) {
        data[pos++] = asdu_len;
    } else {
        data[pos++] = 0x81;
        data[pos++] = asdu_len;
    }
    const char* svid = "WBBOND01";
    int svid_len = 8;
    data[pos++] = 0x80;
    data[pos++] = (uint8_t)svid_len;
    for (int i = 0; i < svid_len; i++) data[pos++] = (uint8_t)svid[i];
    data[pos++] = 0x82; data[pos++] = 0x02;
    data[pos++] = (uint8_t)(smpcnt >> 8);
    data[pos++] = (uint8_t)(smpcnt & 0xFF);
    data[pos++] = 0x83; data[pos++] = 0x04;
    data[pos++] = 0x00; data[pos++] = 0x00; data[pos++] = 0x00; data[pos++] = 0x01;
    data[pos++] = 0x85; data[pos++] = 0x01; data[pos++] = 0x01;
    data[pos++] = 0x87;
    int seqdata_len = num_samples * 8;
    if (seqdata_len < 0x80) {
        data[pos++] = (uint8_t)seqdata_len;
    } else {
        data[pos++] = 0x82;
        data[pos++] = (uint8_t)(seqdata_len >> 8);
        data[pos++] = (uint8_t)(seqdata_len & 0xFF);
    }
    static uint32_t phase_accumulator = 0;
    for (int i = 0; i < num_samples; i++) {
        phase_accumulator += 169;
        int32_t current = (int32_t)(2047.0 * sin(phase_accumulator * 3.1415926535 / 32768.0) * 100.0);
        int32_t voltage = (int32_t)(511.0 * sin((phase_accumulator + 16384) * 3.1415926535 / 32768.0) * 10.0);
        data[pos++] = (uint8_t)(current >> 24);
        data[pos++] = (uint8_t)(current >> 16);
        data[pos++] = (uint8_t)(current >> 8);
        data[pos++] = (uint8_t)(current);
        data[pos++] = (uint8_t)(voltage >> 24);
        data[pos++] = (uint8_t)(voltage >> 16);
        data[pos++] = (uint8_t)(voltage >> 8);
        data[pos++] = (uint8_t)(voltage);
    }
    *out_len = (uint32_t)pos;
}

int capture_simulate(struct capture_handle* h,
                     struct frame_buf* out_batch,
                     int batch_size,
                     int sample_count_per_frame,
                     uint32_t base_smpcnt) {
    if (!h || !out_batch) return -1;
    uint64_t now = mono_ns();
    for (int i = 0; i < batch_size; i++) {
        uint32_t cur_smpcnt = base_smpcnt + (uint32_t)(i * sample_count_per_frame);
        build_sv_frame(out_batch[i].data, &out_batch[i].length, cur_smpcnt, sample_count_per_frame);
        out_batch[i].rx_time_ns = now + (uint64_t)i * (uint64_t)sample_count_per_frame * 10000ULL;
    }
    h->sim_smpcnt += (uint32_t)(batch_size * sample_count_per_frame);
    h->stats.rx_packets += (uint64_t)batch_size;
    uint64_t total_bytes = 0;
    for (int i = 0; i < batch_size; i++) total_bytes += out_batch[i].length;
    h->stats.rx_bytes += total_bytes;
    return batch_size;
}

int capture_get_stats(struct capture_handle* h, struct capture_stats* out_stats) {
    if (!h || !out_stats) return -1;
    memcpy(out_stats, &h->stats, sizeof(*out_stats));
    return 0;
}

void capture_close(struct capture_handle* h) {
    if (!h) return;
#if defined(__linux__)
    if (h->ring_base && h->ring_size) {
        munmap(h->ring_base, h->ring_size);
    }
    if (h->fd >= 0) close(h->fd);
#endif
    free(h);
}

#ifdef _WIN32
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
int clock_gettime_monotonic(struct timespec* ts) {
    static LARGE_INTEGER freq = {0};
    static int init = 0;
    if (!init) {
        QueryPerformanceFrequency(&freq);
        init = 1;
    }
    LARGE_INTEGER now;
    QueryPerformanceCounter(&now);
    ts->tv_sec = (long)(now.QuadPart / freq.QuadPart);
    ts->tv_nsec = (long)(((now.QuadPart % freq.QuadPart) * 1000000000LL) / freq.QuadPart);
    return 0;
}
#endif
