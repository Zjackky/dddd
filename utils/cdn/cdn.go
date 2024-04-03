package cdn

import (
	"dddd/ddout"
	"dddd/structs"
	"dddd/utils"
	"github.com/miekg/dns"
	"github.com/projectdiscovery/gologger"
	"net"
	"strings"
	"sync"
	"time"
)

var domainServers = []string{
	"114.114.114.114:53",
	"223.5.5.5:53",
	"180.76.76.76:53",
	"119.29.29.29:53",
	"182.254.116.116:53",
}

func LookupCNAMEWithServer(domain, domainServer string) ([]string, error) {
	c := dns.Client{
		Timeout: 5 * time.Second,
	}
	var CNAMES []string
	m := dns.Msg{}
	// 最终都会指向一个ip 也就是typeA, 这样就可以返回所有层的cname.
	m.SetQuestion(domain+".", dns.TypeA)
	r, _, err := c.Exchange(&m, domainServer)
	if err != nil {
		return nil, err
	}
	for _, ans := range r.Answer {
		record, isType := ans.(*dns.CNAME)
		if isType {
			CNAMES = append(CNAMES, record.Target)
		}
	}
	return CNAMES, nil
}

func LookupCNAME(domain string) ([]string, error) {
	var lastErr error
	for _, domainServer := range domainServers {
		CNAMES, err := LookupCNAMEWithServer(domain, domainServer)
		if err != nil {
			lastErr = err
		}
		return CNAMES, nil
	}
	return nil, lastErr
}

func CheckCDN(domain string) (bool, string, []net.IP) {
	ips, err := net.LookupIP(domain)
	if err != nil {
		return false, "", []net.IP{}
	}
	for _, ip := range ips {
		for _, item := range ipItems {
			if ip.String() == item.ip {
				return true, item.name, ips
			}
		}
	}

	CNAMES, err := LookupCNAME(domain)
	if err != nil {
		gologger.Error().Msgf("lookupCNAME err: %v", domain)
		return false, "", ips
	}
	for _, cname := range CNAMES {
		for _, item := range domainItems {
			if strings.Contains(cname, item.Domain) {
				return true, item.Name, ips
			}
		}
		if strings.Contains(cname, "cdn") {
			return true, "CNAME关键字:cdn", ips
		}
		if strings.Contains(cname, "waf") {
			return true, "CNAME关键字:waf", ips
		}
	}

	if len(ips) > 1 && len(CNAMES) > 0 {
		return true, "CNAME&&多IP解析", ips
	}

	// 不喜欢这一段可以注释掉，会默认过滤所有IPv6的域名资产
	for _, ip := range ips {
		if utils.IsIPv6(ip.String()) {
			return true, "IPv6", ips
		}
	}

	return false, "", ips
}

func CheckCDNs(domains []string, threads int) (rCDNDomains []string, normalDomains []string, rIPs []string) {
	domains = utils.RemoveDuplicateElement(domains)

	Addrs := make(chan string, len(domains))
	defer close(Addrs)
	results := make(chan structs.CDNResult, len(domains))
	defer close(results)
	var wg sync.WaitGroup

	var rCDNDomainsLock sync.Mutex
	var normalDomainsLock sync.Mutex
	var rIPsLock sync.Mutex

	if threads > len(domains) {
		threads = len(domains)
	}
	gologger.Info().Msgf("开始进行域名CDN识别,数量: %d 线程: %d", len(domains), threads)

	//接收结果
	go func() {
		for result := range results {
			if result.Domain == "" {
				wg.Done()
				continue
			}

			if result.IsCDN {
				// gologger.Silent().Msgf("[CDN-Domain] %v [%v]", result.Domain, result.CDNName)
				ddout.FormatOutput(ddout.OutputMessage{
					Type:          "CDN-Domain",
					IP:            "",
					IPs:           nil,
					Port:          "",
					Protocol:      "",
					Web:           ddout.WebInfo{},
					Finger:        nil,
					Domain:        result.Domain,
					AdditionalMsg: result.CDNName,
				})
				rCDNDomainsLock.Lock()
				rCDNDomains = append(rCDNDomains, result.Domain)
				rCDNDomainsLock.Unlock()
			} else {
				normalDomainsLock.Lock()
				normalDomains = append(normalDomains, result.Domain)
				normalDomainsLock.Unlock()

				for _, each := range result.IPs {
					rIPsLock.Lock()
					rIPs = append(rIPs, each.String())
					rIPsLock.Unlock()
				}
				var ips []string
				if len(result.IPs) != 0 {
					// show := fmt.Sprintf("[RealIP] %v => ", result.Domain)
					for _, ip := range result.IPs {
						ips = append(ips, ip.String())
						structs.GlobalIPDomainMapLock.Lock()
						_, ok := structs.GlobalIPDomainMap[ip.String()]
						structs.GlobalIPDomainMapLock.Unlock()
						if ok {
							// 存在于这个Map中
							structs.GlobalIPDomainMapLock.Lock()
							dms, _ := structs.GlobalIPDomainMap[ip.String()]
							structs.GlobalIPDomainMapLock.Unlock()
							flag := false
							for _, dm := range dms {
								if dm == result.Domain {
									flag = true
									break
								}
							}
							if !flag { // 没有这个域名
								structs.GlobalIPDomainMapLock.Lock()
								structs.GlobalIPDomainMap[ip.String()] = append(structs.GlobalIPDomainMap[ip.String()],
									result.Domain)
								structs.GlobalIPDomainMapLock.Unlock()
							}
						} else {
							structs.GlobalIPDomainMapLock.Lock()
							structs.GlobalIPDomainMap[ip.String()] = []string{result.Domain}
							structs.GlobalIPDomainMapLock.Unlock()
						}
					}

					ddout.FormatOutput(ddout.OutputMessage{
						Type:          "RealIP",
						IP:            "",
						IPs:           ips,
						Port:          "",
						Protocol:      "",
						Web:           ddout.WebInfo{},
						Finger:        nil,
						Domain:        result.Domain,
						AdditionalMsg: "",
					})

				}

			}
			wg.Done()
		}
	}()

	//多线程扫描
	for i := 0; i < threads; i++ {
		go func() {
			for domain := range Addrs {
				isCDN, CDNName, ips := CheckCDN(domain)
				data := structs.CDNResult{
					Domain: domain, IsCDN: isCDN, CDNName: CDNName, IPs: ips,
				}
				results <- data
			}
		}()
	}

	//添加扫描目标
	for _, domain := range domains {
		wg.Add(1)
		Addrs <- domain
	}
	wg.Wait()

	gologger.Info().Msg("CDN识别完毕")
	return rCDNDomains, normalDomains, utils.RemoveDuplicateElement(rIPs)
}

type DomainItem struct {
	Domain      string
	Name        string
	Description string
}

type IPItem struct {
	ip   string
	name string
}

var domainItems = []DomainItem{
	{"15cdn.com", "腾正安全加速（原 15CDN）", "https://www.15cdn.com"},
	{"tzcdn.cn", "腾正安全加速（原 15CDN）", "https://www.15cdn.com"},
	{"cedexis.net", "Cedexis GSLB", "https://www.cedexis.com/"},
	{"cdxcn.cn", "Cedexis GSLB (For China)", "https://www.cedexis.com/"},
	{"qhcdn.com", "360 云 CDN (由奇安信运营)", "https://cloud.360.cn/doc?name=cdn"},
	{"qh-cdn.com", "360 云 CDN (由奇虎 360 运营)", "https://cloud.360.cn/doc?name=cdn"},
	{"qihucdn.com", "360 云 CDN (由奇虎 360 运营)", "https://cloud.360.cn/doc?name=cdn"},
	{"360cdn.com", "360 云 CDN (由奇虎 360 运营)", "https://cloud.360.cn/doc?name=cdn"},
	{"360cloudwaf.com", "奇安信网站卫士", "https://wangzhan.qianxin.com"},
	{"360anyu.com", "奇安信网站卫士", "https://wangzhan.qianxin.com"},
	{"360safedns.com", "奇安信网站卫士", "https://wangzhan.qianxin.com"},
	{"360wzws.com", "奇安信网站卫士", "https://wangzhan.qianxin.com"},
	{"akamai.net", "Akamai CDN", "https://www.akamai.com"},
	{"akamaiedge.net", "Akamai CDN", "https://www.akamai.com"},
	{"ytcdn.net", "Akamai CDN", "https://www.akamai.com"},
	{"edgesuite.net", "Akamai CDN", "https://www.akamai.com"},
	{"akamaitech.net", "Akamai CDN", "https://www.akamai.com"},
	{"akamaitechnologies.com", "Akamai CDN", "https://www.akamai.com"},
	{"edgekey.net", "Akamai CDN", "https://www.akamai.com"},
	{"tl88.net", "易通锐进（Akamai 中国）由网宿承接", "https://www.akamai.com"},
	{"cloudfront.net", "AWS CloudFront", "https://aws.amazon.com/cn/cloudfront/"},
	{"worldcdn.net", "CDN.NET", "https://cdn.net"},
	{"worldssl.net", "CDN.NET / CDNSUN / ONAPP", "https://cdn.net"},
	{"cdn77.org", "CDN77", "https://www.cdn77.com/"},
	{"panthercdn.com", "CDNetworks", "https://www.cdnetworks.com"},
	{"cdnga.net", "CDNetworks", "https://www.cdnetworks.com"},
	{"cdngc.net", "CDNetworks", "https://www.cdnetworks.com"},
	{"gccdn.net", "CDNetworks", "https://www.cdnetworks.com"},
	{"gccdn.cn", "CDNetworks", "https://www.cdnetworks.com"},
	{"akamaized.net", "Akamai CDN", "https://www.akamai.com"},
	{"126.net", "网易云 CDN", "https://www.163yun.com/product/cdn"},
	{"163jiasu.com", "网易云 CDN", "https://www.163yun.com/product/cdn"},
	{"amazonaws.com", "AWS Cloud", "https://aws.amazon.com/cn/cloudfront/"},
	{"cdn77.net", "CDN77", "https://www.cdn77.com/"},
	{"cdnify.io", "CDNIFY", "https://cdnify.com"},
	{"cdnsun.net", "CDNSUN", "https://cdnsun.com"},
	{"bdydns.com", "百度云 CDN", "https://cloud.baidu.com/product/cdn.html"},
	{"ccgslb.com.cn", "蓝汛 CDN", "https://cn.chinacache.com/"},
	{"ccgslb.net", "蓝汛 CDN", "https://cn.chinacache.com/"},
	{"ccgslb.com", "蓝汛 CDN", "https://cn.chinacache.com/"},
	{"ccgslb.cn", "蓝汛 CDN", "https://cn.chinacache.com/"},
	{"c3cache.net", "蓝汛 CDN", "https://cn.chinacache.com/"},
	{"c3dns.net", "蓝汛 CDN", "https://cn.chinacache.com/"},
	{"chinacache.net", "蓝汛 CDN", "https://cn.chinacache.com/"},
	{"wswebcdn.com", "网宿 CDN", "https://www.wangsu.com/"},
	{"lxdns.com", "网宿 CDN", "https://www.wangsu.com/"},
	{"wswebpic.com", "网宿 CDN", "https://www.wangsu.com/"},
	{"cloudflare.net", "Cloudflare", "https://www.cloudflare.com"},
	{"akadns.net", "Akamai CDN", "https://www.akamai.com"},
	{"chinanetcenter.com", "网宿 CDN", "https://www.wangsu.com"},
	{"customcdn.com.cn", "网宿 CDN", "https://www.wangsu.com"},
	{"customcdn.cn", "网宿 CDN", "https://www.wangsu.com"},
	{"51cdn.com", "网宿 CDN", "https://www.wangsu.com"},
	{"wscdns.com", "网宿 CDN", "https://www.wangsu.com"},
	{"cdn20.com", "网宿 CDN", "https://www.wangsu.com"},
	{"wsdvs.com", "网宿 CDN", "https://www.wangsu.com"},
	{"wsglb0.com", "网宿 CDN", "https://www.wangsu.com"},
	{"speedcdns.com", "网宿 CDN", "https://www.wangsu.com"},
	{"wtxcdn.com", "网宿 CDN", "https://www.wangsu.com"},
	{"wsssec.com", "网宿 WAF CDN", "https://www.wangsu.com"},
	{"fastly.net", "Fastly", "https://www.fastly.com"},
	{"fastlylb.net", "Fastly", "https://www.fastly.com/"},
	{"hwcdn.net", "Stackpath (原 Highwinds)", "https://www.stackpath.com/highwinds"},
	{"incapdns.net", "Incapsula CDN", "https://www.incapsula.com"},
	{"kxcdn.com.", "KeyCDN", "https://www.keycdn.com/"},
	{"lswcdn.net", "LeaseWeb CDN", "https://www.leaseweb.com/cdn"},
	{"mwcloudcdn.com", "QUANTIL (网宿)", "https://www.quantil.com/"},
	{"mwcname.com", "QUANTIL (网宿)", "https://www.quantil.com/"},
	{"azureedge.net", "Microsoft Azure CDN", "https://azure.microsoft.com/en-us/services/cdn/"},
	{"msecnd.net", "Microsoft Azure CDN", "https://azure.microsoft.com/en-us/services/cdn/"},
	{"mschcdn.com", "Microsoft Azure CDN", "https://azure.microsoft.com/en-us/services/cdn/"},
	{"v0cdn.net", "Microsoft Azure CDN", "https://azure.microsoft.com/en-us/services/cdn/"},
	{"azurewebsites.net", "Microsoft Azure App Service", "https://azure.microsoft.com/en-us/services/app-service/"},
	{"azurewebsites.windows.net", "Microsoft Azure App Service", "https://azure.microsoft.com/en-us/services/app-service/"},
	{"trafficmanager.net", "Microsoft Azure Traffic Manager", "https://azure.microsoft.com/en-us/services/traffic-manager/"},
	{"cloudapp.net", "Microsoft Azure", "https://azure.microsoft.com"},
	{"chinacloudsites.cn", "世纪互联旗下上海蓝云（承载 Azure 中国）", "https://www.21vbluecloud.com/"},
	{"spdydns.com", "云端智度融合 CDN", "https://www.isurecloud.net/index.html"},
	{"jiashule.com", "知道创宇云安全加速乐CDN", "https://www.yunaq.com/jsl/"},
	{"jiasule.org", "知道创宇云安全加速乐CDN", "https://www.yunaq.com/jsl/"},
	{"365cyd.cn", "知道创宇云安全创宇盾（政务专用）", "https://www.yunaq.com/cyd/"},
	{"huaweicloud.com", "华为云WAF高防云盾", "https://www.huaweicloud.com/product/aad.html"},
	{"cdnhwc1.com", "华为云 CDN", "https://www.huaweicloud.com/product/cdn.html"},
	{"cdnhwc2.com", "华为云 CDN", "https://www.huaweicloud.com/product/cdn.html"},
	{"cdnhwc3.com", "华为云 CDN", "https://www.huaweicloud.com/product/cdn.html"},
	{"dnion.com", "帝联科技", "http://www.dnion.com/"},
	{"ewcache.com", "帝联科技", "http://www.dnion.com/"},
	{"globalcdn.cn", "帝联科技", "http://www.dnion.com/"},
	{"tlgslb.com", "帝联科技", "http://www.dnion.com/"},
	{"fastcdn.com", "帝联科技", "http://www.dnion.com/"},
	{"flxdns.com", "帝联科技", "http://www.dnion.com/"},
	{"dlgslb.cn", "帝联科技", "http://www.dnion.com/"},
	{"newdefend.cn", "牛盾云安全", "https://www.newdefend.com"},
	{"ffdns.net", "CloudXNS", "https://www.cloudxns.net"},
	{"aocdn.com", "可靠云 CDN (贴图库)", "http://www.kekaoyun.com/"},
	{"bsgslb.cn", "白山云 CDN", "https://zh.baishancloud.com/"},
	{"qingcdn.com", "白山云 CDN", "https://zh.baishancloud.com/"},
	{"bsclink.cn", "白山云 CDN", "https://zh.baishancloud.com/"},
	{"trpcdn.net", "白山云 CDN", "https://zh.baishancloud.com/"},
	{"anquan.io", "牛盾云安全", "https://www.newdefend.com"},
	{"cloudglb.com", "快网 CDN", "http://www.fastweb.com.cn/"},
	{"fastweb.com", "快网 CDN", "http://www.fastweb.com.cn/"},
	{"fastwebcdn.com", "快网 CDN", "http://www.fastweb.com.cn/"},
	{"cloudcdn.net", "快网 CDN", "http://www.fastweb.com.cn/"},
	{"fwcdn.com", "快网 CDN", "http://www.fastweb.com.cn/"},
	{"fwdns.net", "快网 CDN", "http://www.fastweb.com.cn/"},
	{"hadns.net", "快网 CDN", "http://www.fastweb.com.cn/"},
	{"hacdn.net", "快网 CDN", "http://www.fastweb.com.cn/"},
	{"cachecn.com", "快网 CDN", "http://www.fastweb.com.cn/"},
	{"qingcache.com", "青云 CDN", "https://www.qingcloud.com/products/cdn/"},
	{"qingcloud.com", "青云 CDN", "https://www.qingcloud.com/products/cdn/"},
	{"frontwize.com", "青云 CDN", "https://www.qingcloud.com/products/cdn/"},
	{"msscdn.com", "美团云 CDN", "https://www.mtyun.com/product/cdn"},
	{"800cdn.com", "西部数码", "https://www.west.cn"},
	{"tbcache.com", "阿里云 CDN", "https://www.aliyun.com/product/cdn"},
	{"aliyun-inc.com", "阿里云 CDN", "https://www.aliyun.com/product/cdn"},
	{"aliyuncs.com", "阿里云 CDN", "https://www.aliyun.com/product/cdn"},
	{"alikunlun.net", "阿里云 CDN", "https://www.aliyun.com/product/cdn"},
	{"alikunlun.com", "阿里云 CDN", "https://www.aliyun.com/product/cdn"},
	{"alicdn.com", "阿里云 CDN", "https://www.aliyun.com/product/cdn"},
	{"aligaofang.com", "阿里云盾高防", "https://www.aliyun.com/product/ddos"},
	{"yundunddos.com", "阿里云盾高防", "https://www.aliyun.com/product/ddos"},
	{"kunlun(.*).com", "阿里云 CDN", "https://www.aliyun.com/product/cdn"},
	{"cdngslb.com", "阿里云 CDN", "https://www.aliyun.com/product/cdn"},
	{"yunjiasu-cdn.net", "百度云加速", "https://su.baidu.com"},
	{"momentcdn.com", "魔门云 CDN", "https://www.cachemoment.com"},
	{"aicdn.com", "又拍云", "https://www.upyun.com"},
	{"qbox.me", "七牛云", "https://www.qiniu.com"},
	{"qiniu.com", "七牛云", "https://www.qiniu.com"},
	{"qiniudns.com", "七牛云", "https://www.qiniu.com"},
	{"jcloudcs.com", "京东云 CDN", "https://www.jdcloud.com/cn/products/cdn"},
	{"jdcdn.com", "京东云 CDN", "https://www.jdcloud.com/cn/products/cdn"},
	{"qianxun.com", "京东云 CDN", "https://www.jdcloud.com/cn/products/cdn"},
	{"jcloudlb.com", "京东云 CDN", "https://www.jdcloud.com/cn/products/cdn"},
	{"jcloud-cdn.com", "京东云 CDN", "https://www.jdcloud.com/cn/products/cdn"},
	{"maoyun.tv", "猫云融合 CDN", "https://www.maoyun.com/"},
	{"maoyundns.com", "猫云融合 CDN", "https://www.maoyun.com/"},
	{"xgslb.net", "WebLuker (蓝汛)", "http://www.webluker.com"},
	{"ucloud.cn", "UCloud CDN", "https://www.ucloud.cn/site/product/ucdn.html"},
	{"ucloud.com.cn", "UCloud CDN", "https://www.ucloud.cn/site/product/ucdn.html"},
	{"cdndo.com", "UCloud CDN", "https://www.ucloud.cn/site/product/ucdn.html"},
	{"zenlogic.net", "Zenlayer CDN", "https://www.zenlayer.com"},
	{"ogslb.com", "Zenlayer CDN", "https://www.zenlayer.com"},
	{"uxengine.net", "Zenlayer CDN", "https://www.zenlayer.com"},
	{"tan14.net", "TAN14 CDN", "http://www.tan14.cn/"},
	{"verycloud.cn", "VeryCloud 云分发", "https://www.verycloud.cn/"},
	{"verycdn.net", "VeryCloud 云分发", "https://www.verycloud.cn/"},
	{"verygslb.com", "VeryCloud 云分发", "https://www.verycloud.cn/"},
	{"xundayun.cn", "SpeedyCloud CDN", "https://www.speedycloud.cn/zh/Products/CDN/CloudDistribution.html"},
	{"xundayun.com", "SpeedyCloud CDN", "https://www.speedycloud.cn/zh/Products/CDN/CloudDistribution.html"},
	{"speedycloud.cc", "SpeedyCloud CDN", "https://www.speedycloud.cn/zh/Products/CDN/CloudDistribution.html"},
	{"mucdn.net", "Verizon CDN (Edgecast)", "https://www.verizondigitalmedia.com/platform/edgecast-cdn/"},
	{"nucdn.net", "Verizon CDN (Edgecast)", "https://www.verizondigitalmedia.com/platform/edgecast-cdn/"},
	{"alphacdn.net", "Verizon CDN (Edgecast)", "https://www.verizondigitalmedia.com/platform/edgecast-cdn/"},
	{"systemcdn.net", "Verizon CDN (Edgecast)", "https://www.verizondigitalmedia.com/platform/edgecast-cdn/"},
	{"edgecastcdn.net", "Verizon CDN (Edgecast)", "https://www.verizondigitalmedia.com/platform/edgecast-cdn/"},
	{"zetacdn.net", "Verizon CDN (Edgecast)", "https://www.verizondigitalmedia.com/platform/edgecast-cdn/"},
	{"coding.io", "Coding Pages", "https://coding.net/pages"},
	{"coding.me", "Coding Pages", "https://coding.net/pages"},
	{"gitlab.io", "GitLab Pages", "https://docs.gitlab.com/ee/user/project/pages/"},
	{"github.io", "GitHub Pages", "https://pages.github.com/"},
	{"herokuapp.com", "Heroku SaaS", "https://www.heroku.com"},
	{"googleapis.com", "Google Cloud Storage", "https://cloud.google.com/storage/"},
	{"netdna.com", "Stackpath (原 MaxCDN)", "https://www.stackpath.com/maxcdn/"},
	{"netdna-cdn.com", "Stackpath (原 MaxCDN)", "https://www.stackpath.com/maxcdn/"},
	{"netdna-ssl.com", "Stackpath (原 MaxCDN)", "https://www.stackpath.com/maxcdn/"},
	{"cdntip.com", "腾讯云 CDN", "https://cloud.tencent.com/product/cdn-scd"},
	{"dnsv1.com", "腾讯云 CDN", "https://cloud.tencent.com/product/cdn-scd"},
	{"tencdns.net", "腾讯云 CDN", "https://cloud.tencent.com/product/cdn-scd"},
	{"dayugslb.com", "腾讯云大禹 BGP 高防", "https://cloud.tencent.com/product/ddos-advanced"},
	{"tcdnvod.com", "腾讯云视频 CDN", "https://lab.skk.moe/cdn"},
	{"tdnsv5.com", "腾讯云 CDN", "https://cloud.tencent.com/product/cdn-scd"},
	{"ksyuncdn.com", "金山云 CDN", "https://www.ksyun.com/post/product/CDN"},
	{"ks-cdn.com", "金山云 CDN", "https://www.ksyun.com/post/product/CDN"},
	{"ksyuncdn-k1.com", "金山云 CDN", "https://www.ksyun.com/post/product/CDN"},
	{"netlify.com", "Netlify", "https://www.netlify.com"},
	{"zeit.co", "ZEIT Now Smart CDN", "https://zeit.co"},
	{"zeit-cdn.net", "ZEIT Now Smart CDN", "https://zeit.co"},
	{"b-cdn.net", "Bunny CDN", "https://bunnycdn.com/"},
	{"lsycdn.com", "蓝视云 CDN", "https://cloud.lsy.cn/"},
	{"scsdns.com", "逸云科技云加速 CDN", "http://www.exclouds.com/navPage/wise"},
	{"quic.cloud", "QUIC.Cloud", "https://quic.cloud/"},
	{"flexbalancer.net", "FlexBalancer - Smart Traffic Routing", "https://perfops.net/flexbalancer"},
	{"gcdn.co", "G - Core Labs", "https://gcorelabs.com/cdn/"},
	{"sangfordns.com", "深信服 AD 系列应用交付产品  单边加速解决方案", "http://www.sangfor.com.cn/topic/2011adn/solutions5.html"},
	{"stspg-customer.com", "StatusPage.io", "https://www.statuspage.io"},
	{"turbobytes.net", "TurboBytes Multi-CDN", "https://www.turbobytes.com"},
	{"turbobytes-cdn.com", "TurboBytes Multi-CDN", "https://www.turbobytes.com"},
	{"att-dsa.net", "AT&T Content Delivery Network", "https://www.business.att.com/products/cdn.html"},
	{"azioncdn.net", "Azion Tech | Edge Computing PLatform", "https://www.azion.com"},
	{"belugacdn.com", "BelugaCDN", "https://www.belugacdn.com"},
	{"cachefly.net", "CacheFly CDN", "https://www.cachefly.com/"},
	{"inscname.net", "Instart CDN", "https://www.instart.com/products/web-performance/cdn"},
	{"insnw.net", "Instart CDN", "https://www.instart.com/products/web-performance/cdn"},
	{"internapcdn.net", "Internap CDN", "https://www.inap.com/network/content-delivery-network"},
	{"footprint.net", "CenturyLink CDN (原 Level 3)", "https://www.centurylink.com/business/networking/cdn.html"},
	{"llnwi.net", "Limelight Network", "https://www.limelight.com"},
	{"llnwd.net", "Limelight Network", "https://www.limelight.com"},
	{"unud.net", "Limelight Network", "https://www.limelight.com"},
	{"lldns.net", "Limelight Network", "https://www.limelight.com"},
	{"stackpathdns.com", "Stackpath CDN", "https://www.stackpath.com"},
	{"stackpathcdn.com", "Stackpath CDN", "https://www.stackpath.com"},
	{"mncdn.com", "Medianova", "https://www.medianova.com"},
	{"rncdn1.com", "Relected Networks", "https://reflected.net/globalcdn"},
	{"simplecdn.net", "Relected Networks", "https://reflected.net/globalcdn"},
	{"swiftserve.com", "Conversant - SwiftServe CDN", "https://reflected.net/globalcdn"},
	{"bitgravity.com", "Tata communications CDN", "https://cdn.tatacommunications.com"},
	{"zenedge.net", "Oracle Dyn Web Application Security suite (原 Zenedge CDN)", "https://cdn.tatacommunications.com"},
	{"biliapi.com", "Bilibili 业务 GSLB", "https://lab.skk.moe/cdn"},
	{"hdslb.net", "Bilibili 高可用负载均衡", "https://github.com/bilibili/overlord"},
	{"hdslb.com", "Bilibili 高可用地域负载均衡", "https://github.com/bilibili/overlord"},
	{"xwaf.cn", "极御云安全（浙江壹云云计算有限公司）", "https://www.stopddos.cn"},
	{"shifen.com", "百度旗下业务地域负载均衡系统", "https://lab.skk.moe/cdn"},
	{"sinajs.cn", "新浪静态域名", "https://lab.skk.moe/cdn"},
	{"tencent-cloud.net", "腾讯旗下业务地域负载均衡系统", "https://lab.skk.moe/cdn"},
	{"elemecdn.com", "饿了么静态域名与地域负载均衡", "https://lab.skk.moe/cdn"},
	{"sinaedge.com", "新浪科技融合CDN负载均衡", "https://lab.skk.moe/cdn"},
	{"sina.com.cn", "新浪科技融合CDN负载均衡", "https://lab.skk.moe/cdn"},
	{"sinacdn.com", "新浪云 CDN", "https://www.sinacloud.com/doc/sae/php/cdn.html"},
	{"sinasws.com", "新浪云 CDN", "https://www.sinacloud.com/doc/sae/php/cdn.html"},
	{"saebbs.com", "新浪云 SAE 云引擎", "https://www.sinacloud.com/doc/sae/php/cdn.html"},
	{"websitecname.cn", "美橙互联旗下建站之星", "https://www.sitestar.cn"},
	{"cdncenter.cn", "美橙互联CDN", "https://www.cndns.com"},
	{"vhostgo.com", "西部数码虚拟主机", "https://www.west.cn"},
	{"jsd.cc", "上海云盾YUNDUN", "https://www.yundun.com"},
	{"powercdn.cn", "动力在线CDN", "http://www.powercdn.com"},
	{"21vokglb.cn", "世纪互联云快线业务", "https://www.21vianet.com"},
	{"21vianet.com.cn", "世纪互联云快线业务", "https://www.21vianet.com"},
	{"21okglb.cn", "世纪互联云快线业务", "https://www.21vianet.com"},
	{"21speedcdn.com", "世纪互联云快线业务", "https://www.21vianet.com"},
	{"21cvcdn.com", "世纪互联云快线业务", "https://www.21vianet.com"},
	{"okcdn.com", "世纪互联云快线业务", "https://www.21vianet.com"},
	{"okglb.com", "世纪互联云快线业务", "https://www.21vianet.com"},
	{"cdnetworks.net", "北京同兴万点网络技术", "http://www.txnetworks.cn/"},
	{"txnetworks.cn", "北京同兴万点网络技术", "http://www.txnetworks.cn/"},
	{"cdnnetworks.com", "北京同兴万点网络技术", "http://www.txnetworks.cn/"},
	{"txcdn.cn", "北京同兴万点网络技术", "http://www.txnetworks.cn/"},
	{"cdnunion.net", "宝腾互联旗下上海万根网络（CDN 联盟）", "http://www.cdnunion.com"},
	{"cdnunion.com", "宝腾互联旗下上海万根网络（CDN 联盟）", "http://www.cdnunion.com"},
	{"mygslb.com", "宝腾互联旗下上海万根网络（YaoCDN）", "http://www.vangen.cn"},
	{"cdnudns.com", "宝腾互联旗下上海万根网络（YaoCDN）", "http://www.vangen.cn"},
	{"sprycdn.com", "宝腾互联旗下上海万根网络（YaoCDN）", "http://www.vangen.cn"},
	{"chuangcdn.com", "创世云融合 CDN", "https://www.chuangcache.com/index.html"},
	{"aocde.com", "创世云融合 CDN", "https://www.chuangcache.com"},
	{"ctxcdn.cn", "中国电信天翼云CDN", "https://www.ctyun.cn/product2/#/product/10027560"},
	{"yfcdn.net", "云帆加速CDN", "https://www.yfcloud.com"},
	{"mmycdn.cn", "蛮蛮云 CDN（中联利信）", "https://www.chinamaincloud.com/cloudDispatch.html"},
	{"chinamaincloud.com", "蛮蛮云 CDN（中联利信）", "https://www.chinamaincloud.com/cloudDispatch.html"},
	{"cnispgroup.com", "中联数据（中联利信）", "http://www.cnispgroup.com/"},
	{"cdnle.com", "新乐视云联（原乐视云）CDN", "http://www.lecloud.com/zh-cn"},
	{"gosuncdn.com", "高升控股CDN技术", "http://www.gosun.com"},
	{"mmtrixopt.com", "mmTrix性能魔方（高升控股旗下）", "http://www.mmtrix.com"},
	{"cloudfence.cn", "蓝盾云CDN", "https://www.cloudfence.cn/#/cloudWeb/yaq/yaqyfx"},
	{"ngaagslb.cn", "新流云（新流万联）", "https://www.ngaa.com.cn"},
	{"p2cdn.com", "星域云P2P CDN", "https://www.xycloud.com"},
	{"00cdn.com", "星域云P2P CDN", "https://www.xycloud.com"},
	{"sankuai.com", "美团云（三快科技）负载均衡", "https://www.mtyun.com"},
	{"lccdn.org", "领智云 CDN（杭州领智云画）", "http://www.linkingcloud.com"},
	{"nscloudwaf.com", "绿盟云 WAF", "https://cloud.nsfocus.com"},
	{"2cname.com", "网堤安全", "https://www.ddos.com"},
	{"ucloudgda.com", "UCloud 罗马 Rome 全球网络加速", "https://www.ucloud.cn/site/product/rome.html"},
	{"google.com", "Google Web 业务", "https://lab.skk.moe/cdn"},
	{"1e100.net", "Google Web 业务", "https://lab.skk.moe/cdn"},
	{"ncname.com", "NodeCache", "https://www.nodecache.com"},
	{"alipaydns.com", "蚂蚁金服旗下业务地域负载均衡系统", "https://lab.skk.moe/cdn/"},
	{"wscloudcdn.com", "全速云（网宿）CloudEdge 云加速", "https://www.quansucloud.com/product.action?product.id=270"},
	{"saaswaf.com", "安恒信息玄武盾", ""},
	{"dbappwaf.cn", "安恒信息玄武盾", ""},
	{"glvs.com", "ALLELINK", ""},
	{"v6lvs.com", "ALLELINK", ""},
	{"haotaomi.com", "ALLELINK", ""},
	{"cloudcdns.com", "云盾CDN", "https://v4.yundun.com/help/112.html"},
	{"yundundns.com", "云盾CDN", ""},
	{"yundunwaf2.com", "阿里云盾", ""},
}

var ipItems = []IPItem{
	{"223.4.77.85", "ALLELINK"},
}
